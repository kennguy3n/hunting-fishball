// Package storage holds the Go storage clients the context engine
// writes to from Stage 4 and reads from in the retrieval API.
//
// Phase 1 lands two clients:
//
//   - QdrantClient — vector store, per-tenant collection. Talks to
//     Qdrant's HTTP REST API (http://qdrant:6333). Future phases swap
//     in github.com/qdrant/go-client gRPC without changing callers.
//
//   - PostgresStore — chunk metadata persisted via GORM. The repository
//     is tenant-scoped at the query layer per ARCHITECTURE.md §5.
//
// Both clients refuse to issue a query without a non-empty tenant_scope,
// per ARCHITECTURE.md §2.4. Cross-tenant reads are *structurally
// impossible*: the call will return ErrMissingTenantScope before the
// transport-level request is built.
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrMissingTenantScope is returned by every storage client method
// when tenant_id is empty. This is the library-boundary tenant guard
// from ARCHITECTURE.md §5.
var ErrMissingTenantScope = errors.New("storage: missing tenant_scope")

// QdrantConfig configures the Qdrant client.
type QdrantConfig struct {
	// BaseURL is the Qdrant REST endpoint, e.g. http://qdrant:6333.
	// Required.
	BaseURL string

	// HTTPClient is used for every Qdrant call. Defaults to a 10s
	// timeout client.
	HTTPClient *http.Client

	// CollectionPrefix is prepended to the per-tenant collection name
	// (e.g. "hf-" → "hf-<tenant_id>"). Empty means raw tenant_id.
	CollectionPrefix string

	// VectorSize is the embedding dimension; the client uses it when
	// auto-creating a collection. Required.
	VectorSize int

	// Distance is the vector distance metric, e.g. "Cosine", "Dot".
	// Defaults to "Cosine".
	Distance string

	// APIKey, when non-empty, is sent in the `api-key` header.
	APIKey string
}

// QdrantPoint is one vector to upsert into Qdrant. ID must be unique
// per (tenant, document, chunk).
type QdrantPoint struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

// QdrantHit is one search result.
type QdrantHit struct {
	ID      string
	Score   float32
	Payload map[string]any
}

// QdrantClient is the per-tenant Qdrant wrapper. The struct itself is
// stateless; tenant scope is supplied per call.
type QdrantClient struct {
	cfg QdrantConfig
}

// NewQdrantClient constructs a QdrantClient. Returns an error if the
// config is malformed.
func NewQdrantClient(cfg QdrantConfig) (*QdrantClient, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("qdrant: BaseURL required")
	}
	if cfg.VectorSize <= 0 {
		return nil, errors.New("qdrant: VectorSize must be > 0")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Distance == "" {
		cfg.Distance = "Cosine"
	}

	return &QdrantClient{cfg: cfg}, nil
}

// CollectionFor returns the collection name for a tenant. Exposed for
// tests and migration tooling.
func (q *QdrantClient) CollectionFor(tenantID string) string {
	return q.cfg.CollectionPrefix + tenantID
}

// EnsureCollection makes sure the per-tenant collection exists. Idempotent.
func (q *QdrantClient) EnsureCollection(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	col := q.CollectionFor(tenantID)

	// Optimistic check first: GET /collections/<name>.
	getURL := q.url("/collections/" + col)
	resp, err := q.do(ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// Create the collection.
	body := map[string]any{
		"vectors": map[string]any{
			"size":     q.cfg.VectorSize,
			"distance": q.cfg.Distance,
		},
	}
	createResp, err := q.do(ctx, http.MethodPut, getURL, body)
	if err != nil {
		return err
	}
	defer func() { _ = createResp.Body.Close() }()

	if createResp.StatusCode != http.StatusOK {
		return q.errFromBody(createResp, "create collection")
	}

	return nil
}

// Upsert writes points into the tenant's collection. Idempotent — Qdrant
// upsert replaces points that share the same ID.
func (q *QdrantClient) Upsert(ctx context.Context, tenantID string, points []QdrantPoint) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if len(points) == 0 {
		return nil
	}
	col := q.CollectionFor(tenantID)

	type qpoint struct {
		ID      string         `json:"id"`
		Vector  []float32      `json:"vector"`
		Payload map[string]any `json:"payload,omitempty"`
	}

	out := make([]qpoint, len(points))
	for i, p := range points {
		// Always tag every point with its tenant_id so retrieval can
		// filter even if the collection is queried directly.
		payload := map[string]any{}
		for k, v := range p.Payload {
			payload[k] = v
		}
		payload["tenant_id"] = tenantID
		out[i] = qpoint{ID: p.ID, Vector: p.Vector, Payload: payload}
	}

	body := map[string]any{"points": out}
	resp, err := q.do(ctx, http.MethodPut, q.url("/collections/"+col+"/points?wait=true"), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return q.errFromBody(resp, "upsert points")
	}

	return nil
}

// SearchOpts narrows a search.
type SearchOpts struct {
	// Limit caps the result count. Defaults to 10.
	Limit int

	// ScoreThreshold filters out matches below the threshold. Zero
	// disables the filter.
	ScoreThreshold float32

	// Filter is a Qdrant filter object. The QdrantClient always ANDs
	// the tenant_id filter so cross-tenant results are impossible.
	Filter map[string]any
}

// Search runs a vector similarity query against the tenant's collection.
// Refuses to run if tenantID is empty (per ARCHITECTURE.md §2.4).
func (q *QdrantClient) Search(ctx context.Context, tenantID string, vector []float32, opts SearchOpts) ([]QdrantHit, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if len(vector) == 0 {
		return nil, errors.New("qdrant: empty vector")
	}
	col := q.CollectionFor(tenantID)

	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}

	// Always include a hard tenant_id filter. The library refuses to
	// query without one (and won't trust the caller's filter alone).
	tenantFilter := map[string]any{
		"must": []map[string]any{
			{"key": "tenant_id", "match": map[string]any{"value": tenantID}},
		},
	}
	filter := tenantFilter
	if opts.Filter != nil {
		// Caller supplied additional clauses; AND them with tenant_id.
		filter = map[string]any{
			"must": []any{tenantFilter, opts.Filter},
		}
	}

	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
		"filter":       filter,
	}
	if opts.ScoreThreshold > 0 {
		body["score_threshold"] = opts.ScoreThreshold
	}

	resp, err := q.do(ctx, http.MethodPost, q.url("/collections/"+col+"/points/search"), body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, q.errFromBody(resp, "search")
	}

	var raw struct {
		Result []struct {
			ID      json.RawMessage `json:"id"`
			Score   float32         `json:"score"`
			Payload map[string]any  `json:"payload"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("qdrant: decode search response: %w", err)
	}

	out := make([]QdrantHit, len(raw.Result))
	for i, r := range raw.Result {
		id := strings.Trim(string(r.ID), `"`)
		out[i] = QdrantHit{ID: id, Score: r.Score, Payload: r.Payload}
	}

	return out, nil
}

// Delete removes points by ID from the tenant's collection.
func (q *QdrantClient) Delete(ctx context.Context, tenantID string, ids []string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if len(ids) == 0 {
		return nil
	}
	col := q.CollectionFor(tenantID)

	body := map[string]any{"points": ids}
	resp, err := q.do(ctx, http.MethodPost, q.url("/collections/"+col+"/points/delete?wait=true"), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return q.errFromBody(resp, "delete points")
	}

	return nil
}

func (q *QdrantClient) url(path string) string {
	u := strings.TrimRight(q.cfg.BaseURL, "/")

	return u + path
}

func (q *QdrantClient) do(ctx context.Context, method, target string, body any) (*http.Response, error) {
	if _, err := url.Parse(target); err != nil {
		return nil, fmt.Errorf("qdrant: parse url: %w", err)
	}

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("qdrant: marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("qdrant: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.cfg.APIKey != "" {
		req.Header.Set("api-key", q.cfg.APIKey)
	}

	resp, err := q.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qdrant: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func (q *QdrantClient) errFromBody(resp *http.Response, op string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	return fmt.Errorf("qdrant: %s: status=%d body=%s", op, resp.StatusCode, body)
}
