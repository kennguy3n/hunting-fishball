// Package retrieval serves the Phase 1 retrieval API: a single
// POST /v1/retrieve endpoint that runs a top-k vector search against
// the per-tenant Qdrant collection and returns ranked results with
// provenance and privacy_label.
//
// Tenant isolation is enforced the same way the audit handler does it
// — tenant_id MUST come from the Gin request context (populated by
// auth middleware) and is never read from the request body or query
// params. The underlying VectorStore also enforces tenant scoping at
// the storage boundary (ErrMissingTenantScope), so even a misconfigured
// handler can't leak across tenants.
package retrieval

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// VectorStore is the narrow contract the retrieval handler needs from
// the underlying vector database. *storage.QdrantClient satisfies it.
type VectorStore interface {
	Search(ctx context.Context, tenantID string, vector []float32, opts storage.SearchOpts) ([]storage.QdrantHit, error)
}

// Embedder produces a single-query embedding. The retrieval handler
// uses it to convert the user's text query into a vector.
type Embedder interface {
	EmbedQuery(ctx context.Context, tenantID, query string) ([]float32, error)
}

// HandlerConfig configures the retrieval Handler.
type HandlerConfig struct {
	VectorStore VectorStore
	Embedder    Embedder

	// DefaultTopK is the default top_k when the request doesn't set
	// one. Defaults to 10.
	DefaultTopK int

	// MaxTopK caps the user-supplied top_k. Defaults to 100.
	MaxTopK int
}

// Handler serves the retrieval HTTP API.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates cfg and returns a Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.VectorStore == nil {
		return nil, errors.New("retrieval: nil VectorStore")
	}
	if cfg.Embedder == nil {
		return nil, errors.New("retrieval: nil Embedder")
	}
	if cfg.DefaultTopK == 0 {
		cfg.DefaultTopK = 10
	}
	if cfg.MaxTopK == 0 {
		cfg.MaxTopK = 100
	}

	return &Handler{cfg: cfg}, nil
}

// Register mounts the retrieval endpoints on rg. Routes:
//
//	POST /v1/retrieve
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/retrieve", h.retrieve)
}

// RetrieveRequest is the JSON shape of POST /v1/retrieve.
type RetrieveRequest struct {
	Query     string   `json:"query" binding:"required"`
	TopK      int      `json:"top_k"`
	Sources   []string `json:"sources,omitempty"`
	Channels  []string `json:"channels,omitempty"`
	Documents []string `json:"document_ids,omitempty"`
}

// RetrieveHit is one entry in the response.
type RetrieveHit struct {
	ID           string         `json:"id"`
	Score        float32        `json:"score"`
	TenantID     string         `json:"tenant_id"`
	SourceID     string         `json:"source_id,omitempty"`
	DocumentID   string         `json:"document_id,omitempty"`
	BlockID      string         `json:"block_id,omitempty"`
	Title        string         `json:"title,omitempty"`
	URI          string         `json:"uri,omitempty"`
	PrivacyLabel string         `json:"privacy_label,omitempty"`
	Text         string         `json:"text,omitempty"`
	Connector    string         `json:"connector,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// RetrieveResponse is the JSON envelope for POST /v1/retrieve.
type RetrieveResponse struct {
	Hits []RetrieveHit `json:"hits"`
}

func (h *Handler) retrieve(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}

	var req RetrieveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})

		return
	}
	if req.Query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query is required"})

		return
	}
	topK := req.TopK
	if topK == 0 {
		topK = h.cfg.DefaultTopK
	}
	if topK < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "top_k must be >= 0"})

		return
	}
	if topK > h.cfg.MaxTopK {
		topK = h.cfg.MaxTopK
	}

	vec, err := h.cfg.Embedder.EmbedQuery(c.Request.Context(), tenantID, req.Query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "embed failed"})

		return
	}

	filter := buildFilter(req)

	hits, err := h.cfg.VectorStore.Search(c.Request.Context(), tenantID, vec, storage.SearchOpts{
		Limit:  topK,
		Filter: filter,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed"})

		return
	}

	out := RetrieveResponse{Hits: make([]RetrieveHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, hitFromQdrant(h))
	}
	c.JSON(http.StatusOK, out)
}

// tenantIDFromContext mirrors the audit-handler implementation so the
// authentication contract is identical across handlers.
func tenantIDFromContext(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}

	return s, true
}

// buildFilter projects the optional request fields into a Qdrant
// filter object. Returns nil if no filters were set.
func buildFilter(req RetrieveRequest) map[string]any {
	conds := []map[string]any{}
	add := func(key string, vals []string) {
		if len(vals) == 0 {
			return
		}
		any_ := []map[string]any{}
		for _, v := range vals {
			any_ = append(any_, map[string]any{"key": key, "match": map[string]any{"value": v}})
		}
		conds = append(conds, map[string]any{"should": any_})
	}
	add("source_id", req.Sources)
	add("namespace_id", req.Channels)
	add("document_id", req.Documents)
	if len(conds) == 0 {
		return nil
	}

	return map[string]any{"must": conds}
}

// hitFromQdrant projects the Qdrant payload into a stable response
// shape. Unknown payload keys are surfaced under metadata so callers
// can inspect them without us having to update this code on every
// connector change.
func hitFromQdrant(h storage.QdrantHit) RetrieveHit {
	out := RetrieveHit{
		ID:    h.ID,
		Score: h.Score,
	}
	known := map[string]struct{}{
		"tenant_id":     {},
		"source_id":     {},
		"document_id":   {},
		"block_id":      {},
		"title":         {},
		"uri":           {},
		"privacy_label": {},
		"text":          {},
		"connector":     {},
	}
	for k, v := range h.Payload {
		switch k {
		case "tenant_id":
			out.TenantID, _ = v.(string)
		case "source_id":
			out.SourceID, _ = v.(string)
		case "document_id":
			out.DocumentID, _ = v.(string)
		case "block_id":
			out.BlockID, _ = v.(string)
		case "title":
			out.Title, _ = v.(string)
		case "uri":
			out.URI, _ = v.(string)
		case "privacy_label":
			out.PrivacyLabel, _ = v.(string)
		case "text":
			out.Text, _ = v.(string)
		case "connector":
			out.Connector, _ = v.(string)
		}
	}
	for k, v := range h.Payload {
		if _, ok := known[k]; ok {
			continue
		}
		if out.Metadata == nil {
			out.Metadata = map[string]any{}
		}
		out.Metadata[k] = v
	}

	return out
}
