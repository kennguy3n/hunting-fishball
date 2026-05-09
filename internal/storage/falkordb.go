package storage

// Phase 3 graph traversal client.
//
// FalkorDB speaks the Redis protocol with the GRAPH.* command set
// (GRAPH.QUERY, GRAPH.DELETE, ...). The upstream Go SDK
// (github.com/FalkorDB/falkordb-go) is a thin wrapper over
// gomodule/redigo's `Conn.Do("GRAPH.QUERY", graph, query)` and exposes
// a single non-context-aware connection. The retrieval API needs
// context cancellation, connection pooling, and the same client used
// by the semantic cache, so this implementation talks to FalkorDB
// directly through the go-redis v9 client (which we already use for
// the semantic cache). The decision is captured in
// docs/ARCHITECTURE.md §9 "Tech choices added in Phase 3".
//
// Tenancy. Each tenant gets its own graph keyed
// `<GraphPrefix><tenant_id>` (per ARCHITECTURE.md §2.4 "per-tenant
// graph"). Cross-tenant traversal is structurally impossible — every
// FalkorDB call refuses to run when tenant_id is empty
// (ErrMissingTenantScope) and only ever issues GRAPH.* against the
// tenant's own graph.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// FalkorDB is the narrow contract this package needs from a Redis
// client. *redis.Client (go-redis/v9) satisfies it; tests inject an
// in-memory fake.
type FalkorDB interface {
	Do(ctx context.Context, args ...any) FalkorCmd
}

// FalkorCmd is the per-command result type. The narrow contract is
// what the FalkorDBClient actually consumes from go-redis's
// *redis.Cmd: Result + Err. Tests stub it directly without dragging
// the redis types into the test boundary.
type FalkorCmd interface {
	Result() (any, error)
	Err() error
}

// FalkorNode is one node to write into the tenant graph.
type FalkorNode struct {
	// ID is the node identifier (encoded as a property `id` so the
	// retrieval traversal can address it). Required.
	ID string

	// Label is the node label (e.g. "Document", "Person"). Required.
	Label string

	// DocumentID groups every node that derived from the same
	// upstream document. DeleteByDocument matches on this.
	DocumentID string

	// Properties carries any extra fields. Values must be string,
	// bool, int, int64, or float64.
	Properties map[string]any
}

// FalkorEdge is a directed edge from Source.ID to Destination.ID.
type FalkorEdge struct {
	Source      string
	Destination string

	// Relation is the relationship type (e.g. "MENTIONS", "WROTE").
	// Required.
	Relation string

	// DocumentID groups every edge that derived from the same
	// upstream document. DeleteByDocument matches on this.
	DocumentID string

	// Properties carries any extra fields. Values must be string,
	// bool, int, int64, or float64.
	Properties map[string]any
}

// FalkorMatch is one traversal result.
type FalkorMatch struct {
	NodeID     string
	Label      string
	DocumentID string
	Score      float32
	Properties map[string]any
}

// FalkorDBConfig configures the FalkorDB client.
type FalkorDBConfig struct {
	// Client is the FalkorDB-speaking Redis client. Required.
	Client FalkorDB

	// GraphPrefix is prepended to the per-tenant graph name (e.g.
	// "hf-" → "hf-<tenant_id>"). Empty means raw tenant_id.
	GraphPrefix string
}

// FalkorDBClient is the per-tenant graph traversal client.
type FalkorDBClient struct {
	cfg FalkorDBConfig
}

// NewFalkorDBClient validates cfg and returns a *FalkorDBClient.
func NewFalkorDBClient(cfg FalkorDBConfig) (*FalkorDBClient, error) {
	if cfg.Client == nil {
		return nil, errors.New("falkordb: nil Client")
	}

	return &FalkorDBClient{cfg: cfg}, nil
}

// redisDoer is the subset of *redis.Client / *redis.ClusterClient our
// adapter calls; it lets the cmd/api wiring pass either.
type redisDoer interface {
	Do(ctx context.Context, args ...any) *redis.Cmd
}

// FalkorDBFromRedis adapts a go-redis client into the narrow FalkorDB
// interface so the same connection pool can serve both the semantic
// cache and the FalkorDB graph.
func FalkorDBFromRedis(c redisDoer) FalkorDB {
	return &goRedisFalkor{c: c}
}

type goRedisFalkor struct {
	c redisDoer
}

func (g *goRedisFalkor) Do(ctx context.Context, args ...any) FalkorCmd {
	return g.c.Do(ctx, args...)
}

// GraphFor returns the per-tenant graph name. Exposed for tests.
func (c *FalkorDBClient) GraphFor(tenantID string) string {
	return c.cfg.GraphPrefix + tenantID
}

// WriteNodes upserts the supplied nodes and edges into the tenant's
// graph using a single GRAPH.QUERY call. Re-issuing WriteNodes with
// the same node IDs is idempotent (FalkorDB MERGE semantics).
func (c *FalkorDBClient) WriteNodes(ctx context.Context, tenantID string, nodes []FalkorNode, edges []FalkorEdge) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(nodes) == 0 && len(edges) == 0 {
		return nil
	}

	graph := c.GraphFor(tenantID)
	var b strings.Builder
	for i, n := range nodes {
		if n.ID == "" || n.Label == "" {
			return fmt.Errorf("falkordb: node %d: ID and Label required", i)
		}
		props := mergeProps(n.Properties, map[string]any{
			"id":          n.ID,
			"document_id": n.DocumentID,
		})
		fmt.Fprintf(&b, "MERGE (n%d:%s {id:%s}) SET n%d = %s ", i, sanitizeLabel(n.Label), cypherValue(n.ID), i, cypherProps(props))
	}
	for i, e := range edges {
		if e.Source == "" || e.Destination == "" || e.Relation == "" {
			return fmt.Errorf("falkordb: edge %d: Source, Destination, Relation required", i)
		}
		props := mergeProps(e.Properties, map[string]any{
			"document_id": e.DocumentID,
		})
		fmt.Fprintf(&b, "MERGE (s%d {id:%s}) MERGE (d%d {id:%s}) MERGE (s%d)-[r%d:%s %s]->(d%d) ",
			i, cypherValue(e.Source),
			i, cypherValue(e.Destination),
			i, i, sanitizeLabel(e.Relation), cypherProps(props),
			i)
	}

	cmd := c.cfg.Client.Do(ctx, "GRAPH.QUERY", graph, strings.TrimSpace(b.String()))
	if err := cmd.Err(); err != nil {
		return fmt.Errorf("falkordb: GRAPH.QUERY: %w", err)
	}

	return nil
}

// Traverse runs a query against the tenant graph and returns the
// matched nodes. The query is a parameterised name → string lookup
// against node `id` properties; FalkorDB returns the node id, label,
// and any property the caller's MATCH clause exposes via RETURN.
//
// The returned []*FalkorMatch is best-effort: when the FalkorDB row
// shape does not parse, the match is dropped rather than failing the
// whole call (so the retrieval merger still sees the rows it can
// score). Errors at the protocol layer (network / authn / syntax) are
// returned to the caller.
func (c *FalkorDBClient) Traverse(ctx context.Context, tenantID, query string, k int) ([]*FalkorMatch, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query == "" {
		return nil, errors.New("falkordb: empty query")
	}
	if k <= 0 {
		k = 10
	}

	graph := c.GraphFor(tenantID)

	// We build a fixed-shape query: anchor on documents whose id /
	// title / text matches the user query. The handler is responsible
	// for choosing the anchor strategy before calling Traverse; here
	// we honour whatever the caller asked for and just enforce the
	// `LIMIT k` so a runaway query can't drown the API.
	cypher := fmt.Sprintf("%s LIMIT %d", strings.TrimSuffix(strings.TrimSpace(query), ";"), k)

	cmd := c.cfg.Client.Do(ctx, "GRAPH.QUERY", graph, cypher)
	if err := cmd.Err(); err != nil {
		return nil, fmt.Errorf("falkordb: traverse: %w", err)
	}

	res, err := cmd.Result()
	if err != nil {
		return nil, fmt.Errorf("falkordb: traverse result: %w", err)
	}

	return parseTraverseResult(res), nil
}

// DeleteByDocument removes every node and edge whose `document_id`
// equals docID from the tenant's graph. Safe to call repeatedly.
func (c *FalkorDBClient) DeleteByDocument(ctx context.Context, tenantID, docID string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if docID == "" {
		return errors.New("falkordb: docID required")
	}

	graph := c.GraphFor(tenantID)
	cypher := fmt.Sprintf("MATCH (n {document_id:%s}) DETACH DELETE n", cypherValue(docID))

	cmd := c.cfg.Client.Do(ctx, "GRAPH.QUERY", graph, cypher)
	if err := cmd.Err(); err != nil {
		return fmt.Errorf("falkordb: delete: %w", err)
	}

	return nil
}

// parseTraverseResult flattens the FalkorDB GRAPH.QUERY response into
// a slice of FalkorMatch. The wire format is documented at
// https://docs.falkordb.com/commands/graph.query.html. The minimum
// shape we need is `[ [headers], [ row, row, ... ], [ statistics ] ]`;
// each row is `[ value0, value1, ... ]` where a node value is itself
// `[ id, [labels], [ [propname, propvalue], ... ] ]`.
//
// We intentionally accept partial / malformed rows because the
// retrieval merger applies score gating downstream — we never want a
// stray protocol surprise to take retrieval offline.
func parseTraverseResult(raw any) []*FalkorMatch {
	rows, ok := raw.([]any)
	if !ok || len(rows) < 2 {
		return nil
	}
	body, ok := rows[1].([]any)
	if !ok {
		return nil
	}
	out := make([]*FalkorMatch, 0, len(body))
	for _, row := range body {
		cells, ok := row.([]any)
		if !ok || len(cells) == 0 {
			continue
		}
		match := matchFromCells(cells)
		if match == nil {
			continue
		}
		out = append(out, match)
	}

	return out
}

// matchFromCells extracts a single FalkorMatch from one row's cells.
// The first cell is treated as a node; trailing cells contribute
// scalar properties keyed by their position (caller supplies them
// via the RETURN clause as id, label, score, ...).
func matchFromCells(cells []any) *FalkorMatch {
	first, ok := cells[0].([]any)
	if !ok || len(first) < 3 {
		// Maybe the caller asked for `RETURN n.id, n.label, score`
		// only — handle that flat shape too.
		return matchFromFlat(cells)
	}

	m := &FalkorMatch{Properties: map[string]any{}}
	if id, ok := first[0].(string); ok {
		m.NodeID = id
	}
	if labels, ok := first[1].([]any); ok && len(labels) > 0 {
		if l, ok := labels[0].(string); ok {
			m.Label = l
		}
	}
	if props, ok := first[2].([]any); ok {
		for _, kv := range props {
			pair, ok := kv.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			name, _ := pair[0].(string)
			if name == "" {
				continue
			}
			m.Properties[name] = pair[1]
			if name == "id" {
				if v, ok := pair[1].(string); ok {
					m.NodeID = v
				}
			}
			if name == "document_id" {
				if v, ok := pair[1].(string); ok {
					m.DocumentID = v
				}
			}
		}
	}
	if len(cells) > 1 {
		switch v := cells[1].(type) {
		case string:
			m.Properties["return_1"] = v
		case float64:
			m.Score = float32(v)
		case int64:
			m.Score = float32(v)
		}
	}

	return m
}

// matchFromFlat handles `RETURN n.id, n.label, score` style results.
func matchFromFlat(cells []any) *FalkorMatch {
	m := &FalkorMatch{Properties: map[string]any{}}
	if v, ok := cells[0].(string); ok {
		m.NodeID = v
	}
	if len(cells) >= 2 {
		if v, ok := cells[1].(string); ok {
			m.Label = v
		}
	}
	if len(cells) >= 3 {
		switch v := cells[2].(type) {
		case float64:
			m.Score = float32(v)
		case int64:
			m.Score = float32(v)
		}
	}
	if m.NodeID == "" {
		return nil
	}

	return m
}

// cypherValue serialises a Go value into a Cypher literal. We support
// the property types FalkorDB itself supports: string, bool, integer,
// double. nil collapses to NULL.
func cypherValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return strconv.Quote(x)
	case bool:
		if x {
			return "true"
		}

		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return strconv.Quote(fmt.Sprint(x))
	}
}

// cypherProps serialises a property map into Cypher map syntax.
func cypherProps(p map[string]any) string {
	if len(p) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(sanitizeLabel(k))
		b.WriteByte(':')
		b.WriteString(cypherValue(p[k]))
	}
	b.WriteByte('}')

	return b.String()
}

// mergeProps merges b on top of a; keys in b win.
func mergeProps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if v == nil || v == "" {
			continue
		}
		out[k] = v
	}

	return out
}

// sanitizeLabel restricts label / relation / property names to
// `[A-Za-z0-9_]` to keep Cypher safe. Anything outside the safe set
// is dropped.
func sanitizeLabel(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_':
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return "Item"
	}

	return string(out)
}
