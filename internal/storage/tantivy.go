// Package storage Phase 3 BM25 client.
//
// The retrieval architecture (ARCHITECTURE.md §4.2) names tantivy-go as
// the BM25 backend. As of Phase 3 the upstream tantivy-go cgo bindings
// are still on a v0.x track that requires Rust + libstdc++ at build
// time, which collides with the rest of the engine's "go-only build,
// pure-Go test cache" invariant. We pick the well-supported pure-Go
// BM25 engine [bleve] as the implementation behind a `TantivyClient`
// interface; if a stable cgo-free tantivy-go ever lands the swap is
// behind this single file.
//
// Decision: pure-Go BM25 (bleve). See docs/ARCHITECTURE.md §9 "Tech
// choices added in Phase 3".
//
// Tenancy. Each tenant gets its own on-disk index directory rooted at
// `<RootDir>/<tenant_id>` (per ARCHITECTURE.md §2.4 "per-tenant
// directory" convention). Cross-tenant queries are structurally
// impossible — the client refuses to issue any operation without a
// non-empty tenant_id (ErrMissingTenantScope) and only opens the
// tenant's own directory.
package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
)

// BM25Chunk is the unit indexed by the BM25 client. The fields mirror
// the subset of storage.Chunk that the BM25 stage actually scores.
type BM25Chunk struct {
	// ID is the canonical chunk identifier (also used as the Qdrant
	// point ID and the Postgres chunks.id). Required.
	ID string

	// DocumentID groups chunks belonging to the same upstream
	// document. The Delete method drops every chunk for a given
	// (tenant_id, document_id) pair.
	DocumentID string

	// Title is the document title (boosted at search time).
	Title string

	// Text is the canonical chunk body (the BM25-scored field).
	Text string
}

// BM25Match is one search result produced by the BM25 client. It
// shares the same shape as the other backends' Match types so the
// retrieval merger can fuse scores across streams.
type BM25Match struct {
	ID         string
	DocumentID string
	Score      float32
	Title      string
	Text       string
}

// TantivyConfig configures the BM25 client.
type TantivyConfig struct {
	// RootDir is the parent directory under which per-tenant index
	// directories are created. Required.
	RootDir string

	// IndexMapping, when non-nil, overrides the default mapping. Tests
	// use it to disable persistence (in-memory bleve.NewMemOnly).
	IndexMapping mapping.IndexMapping

	// MemOnly, when true, opens an in-memory index per tenant. Used
	// by unit tests and the integration test suite to avoid touching
	// disk.
	MemOnly bool
}

// TantivyClient is the BM25 backend used by the retrieval API.
//
// The struct is goroutine-safe; per-tenant indexes are opened lazily
// and cached behind a sync.Mutex.
type TantivyClient struct {
	cfg     TantivyConfig
	mu      sync.Mutex
	indexes map[string]bleve.Index // keyed by tenant_id
}

// NewTantivyClient returns a TantivyClient backed by bleve. RootDir is
// required unless MemOnly is true.
func NewTantivyClient(cfg TantivyConfig) (*TantivyClient, error) {
	if !cfg.MemOnly && cfg.RootDir == "" {
		return nil, errors.New("tantivy: RootDir is required when MemOnly is false")
	}

	return &TantivyClient{
		cfg:     cfg,
		indexes: make(map[string]bleve.Index),
	}, nil
}

// Close flushes and releases every open per-tenant index. Always call
// Close before exiting the process.
func (c *TantivyClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var firstErr error
	for tenantID, idx := range c.indexes {
		if err := idx.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("tantivy: close %s: %w", tenantID, err)
		}
		delete(c.indexes, tenantID)
	}

	return firstErr
}

// Index writes the supplied chunks into the tenant's BM25 index. The
// chunk ID is the bleve document ID; re-indexing the same ID upserts.
func (c *TantivyClient) Index(ctx context.Context, tenantID string, chunks []BM25Chunk) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}

	idx, err := c.indexFor(tenantID)
	if err != nil {
		return err
	}

	batch := idx.NewBatch()
	for _, ch := range chunks {
		if ch.ID == "" {
			return errors.New("tantivy: chunk ID is required")
		}
		doc := map[string]any{
			"document_id": ch.DocumentID,
			"title":       ch.Title,
			"text":        ch.Text,
		}
		if err := batch.Index(ch.ID, doc); err != nil {
			return fmt.Errorf("tantivy: batch index %s: %w", ch.ID, err)
		}
	}

	return idx.Batch(batch)
}

// Search runs a BM25 query against the tenant's index and returns
// the top-k matches. Empty query returns an empty result list.
func (c *TantivyClient) Search(ctx context.Context, tenantID, q string, k int) ([]*BM25Match, error) {
	if tenantID == "" {
		return nil, ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		k = 10
	}
	if q == "" {
		return nil, nil
	}

	idx, err := c.indexFor(tenantID)
	if err != nil {
		return nil, err
	}

	// Boolean OR of two MatchQuerys — title-boosted and body —
	// gives bleve the same shape tantivy's `BoostedQuery` would. We
	// avoid the query-string parser so user-supplied operators
	// ("title:foo", "AND", "OR", ...) cannot inject field-scoped
	// queries that escape the tenant boundary.
	titleQ := bleve.NewMatchQuery(q)
	titleQ.SetField("title")
	titleQ.SetBoost(2.0)
	textQ := bleve.NewMatchQuery(q)
	textQ.SetField("text")

	should := query.NewDisjunctionQuery([]query.Query{titleQ, textQ})

	req := bleve.NewSearchRequestOptions(should, k, 0, false)
	req.Fields = []string{"document_id", "title", "text"}

	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("tantivy: search: %w", err)
	}

	matches := make([]*BM25Match, 0, len(res.Hits))
	for _, h := range res.Hits {
		m := &BM25Match{
			ID:    h.ID,
			Score: float32(h.Score),
		}
		if v, ok := h.Fields["document_id"].(string); ok {
			m.DocumentID = v
		}
		if v, ok := h.Fields["title"].(string); ok {
			m.Title = v
		}
		if v, ok := h.Fields["text"].(string); ok {
			m.Text = v
		}
		matches = append(matches, m)
	}

	return matches, nil
}

// Delete removes every chunk that belongs to (tenantID, docID) from
// the tenant's BM25 index. Idempotent.
func (c *TantivyClient) Delete(ctx context.Context, tenantID, docID string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if docID == "" {
		return errors.New("tantivy: docID is required")
	}

	idx, err := c.indexFor(tenantID)
	if err != nil {
		return err
	}

	q := query.NewTermQuery(docID)
	q.SetField("document_id")
	req := bleve.NewSearchRequest(q)
	req.Size = 1000
	res, err := idx.SearchInContext(ctx, req)
	if err != nil {
		return fmt.Errorf("tantivy: lookup for delete: %w", err)
	}
	if len(res.Hits) == 0 {
		return nil
	}

	batch := idx.NewBatch()
	for _, h := range res.Hits {
		batch.Delete(h.ID)
	}

	return idx.Batch(batch)
}

// indexFor opens (or returns the cached) bleve index for tenantID.
func (c *TantivyClient) indexFor(tenantID string) (bleve.Index, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if idx, ok := c.indexes[tenantID]; ok {
		return idx, nil
	}

	mapping := c.cfg.IndexMapping
	if mapping == nil {
		mapping = defaultBM25Mapping()
	}

	var (
		idx bleve.Index
		err error
	)
	if c.cfg.MemOnly {
		idx, err = bleve.NewMemOnly(mapping)
	} else {
		path := filepath.Join(c.cfg.RootDir, tenantID)
		idx, err = bleve.Open(path)
		if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
			idx, err = bleve.New(path, mapping)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("tantivy: open index for %s: %w", tenantID, err)
	}

	c.indexes[tenantID] = idx

	return idx, nil
}

// defaultBM25Mapping returns the index mapping used when callers do
// not supply one.
func defaultBM25Mapping() mapping.IndexMapping {
	textField := bleve.NewTextFieldMapping()
	textField.Store = true
	textField.IncludeInAll = true

	docMapping := bleve.NewDocumentMapping()
	docMapping.AddFieldMappingsAt("text", textField)
	docMapping.AddFieldMappingsAt("title", textField)
	docMapping.AddFieldMappingsAt("document_id", bleve.NewKeywordFieldMapping())

	im := bleve.NewIndexMapping()
	im.DefaultMapping = docMapping

	return im
}
