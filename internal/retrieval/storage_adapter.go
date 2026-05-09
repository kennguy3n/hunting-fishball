package retrieval

// Phase 3 storage → retrieval adapters. These are tiny projections
// from the storage-layer match types (BM25Match, FalkorMatch) into
// retrieval.Match so the fan-out handler stays decoupled from the
// concrete storage clients. Each adapter exposes a Search /
// Traverse method matching the BM25Backend / GraphBackend interface.

import (
	"context"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// TantivyAdapter wraps *storage.TantivyClient as a BM25Backend.
type TantivyAdapter struct {
	Client *storage.TantivyClient
}

// Search implements BM25Backend.
func (a *TantivyAdapter) Search(ctx context.Context, tenantID, q string, k int) ([]*Match, error) {
	hits, err := a.Client.Search(ctx, tenantID, q, k)
	if err != nil {
		return nil, err
	}
	out := make([]*Match, 0, len(hits))
	for i, h := range hits {
		out = append(out, &Match{
			ID:         h.ID,
			Source:     SourceBM25,
			Score:      h.Score,
			Rank:       i + 1,
			DocumentID: h.DocumentID,
			Title:      h.Title,
			Text:       h.Text,
		})
	}

	return out, nil
}

// FalkorAdapter wraps *storage.FalkorDBClient as a GraphBackend.
type FalkorAdapter struct {
	Client *storage.FalkorDBClient
	// QueryTemplate, when non-empty, formats the user's query into a
	// Cypher MATCH. The default is a label-anchored full-text scan
	// over `name` properties.
	QueryTemplate func(query string) string
}

// Traverse implements GraphBackend.
func (a *FalkorAdapter) Traverse(ctx context.Context, tenantID, query string, k int) ([]*Match, error) {
	tmpl := a.QueryTemplate
	if tmpl == nil {
		tmpl = defaultGraphQuery
	}
	hits, err := a.Client.Traverse(ctx, tenantID, tmpl(query), k)
	if err != nil {
		return nil, err
	}
	out := make([]*Match, 0, len(hits))
	for i, h := range hits {
		m := &Match{
			ID:         h.NodeID,
			Source:     SourceGraph,
			Score:      h.Score,
			Rank:       i + 1,
			DocumentID: h.DocumentID,
		}
		if t, ok := h.Properties["title"].(string); ok {
			m.Title = t
		}
		if u, ok := h.Properties["uri"].(string); ok {
			m.URI = u
		}
		if l, ok := h.Properties["privacy_label"].(string); ok {
			m.PrivacyLabel = l
		}
		out = append(out, m)
	}

	return out, nil
}

// defaultGraphQuery returns a generic anchor: any node whose `name`
// or `title` contains the user query. Real deployments override this
// via FalkorAdapter.QueryTemplate to build entity-anchored
// multi-hop queries.
func defaultGraphQuery(q string) string {
	// Embed the query as a Cypher string literal.
	safe := make([]byte, 0, len(q)+2)
	safe = append(safe, '"')
	for i := 0; i < len(q); i++ {
		ch := q[i]
		if ch == '"' || ch == '\\' {
			safe = append(safe, '\\')
		}
		safe = append(safe, ch)
	}
	safe = append(safe, '"')
	str := string(safe)

	return "MATCH (n) WHERE n.title CONTAINS " + str + " OR n.name CONTAINS " + str + " RETURN n"
}
