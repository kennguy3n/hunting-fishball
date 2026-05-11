// chunk_quality_handler.go — Round-7 Task 12.
//
// GET /v1/admin/chunks/quality-report aggregates chunk-quality
// rows (written by the Stage-4 chunk_scorer pre-write hook) into
// a per-source distribution. The dashboard surfaces this so
// operators can see which sources are producing low-quality
// chunks.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// ChunkQualityRow is the persisted row for one chunk.
type ChunkQualityRow struct {
	TenantID     string    `gorm:"column:tenant_id;type:char(26);primaryKey" json:"tenant_id"`
	SourceID     string    `gorm:"column:source_id;type:char(26)" json:"source_id"`
	DocumentID   string    `gorm:"column:document_id" json:"document_id"`
	ChunkID      string    `gorm:"column:chunk_id;primaryKey" json:"chunk_id"`
	QualityScore float64   `gorm:"column:quality_score" json:"quality_score"`
	LengthScore  float64   `gorm:"column:length_score" json:"length_score"`
	LangScore    float64   `gorm:"column:lang_score" json:"lang_score"`
	EmbedScore   float64   `gorm:"column:embed_score" json:"embed_score"`
	Duplicate    bool      `gorm:"column:duplicate" json:"duplicate"`
	UpdatedAt    time.Time `gorm:"column:updated_at" json:"updated_at"`
}

// TableName overrides GORM pluralization.
func (ChunkQualityRow) TableName() string { return "chunk_quality" }

// ChunkQualityReader is the narrow read port the report needs.
type ChunkQualityReader interface {
	ListByTenant(ctx context.Context, tenantID string, limit int) ([]ChunkQualityRow, error)
}

// SourceBucket is the per-source aggregate.
type SourceBucket struct {
	SourceID  string  `json:"source_id"`
	Count     int     `json:"count"`
	AvgScore  float64 `json:"avg_score"`
	BelowHalf int     `json:"below_half"`
	Duplicate int     `json:"duplicate"`
}

// ChunkQualityReport is the JSON envelope.
type ChunkQualityReport struct {
	TenantID string         `json:"tenant_id"`
	Buckets  []SourceBucket `json:"buckets"`
}

// ChunkQualityHandler is the HTTP surface.
type ChunkQualityHandler struct {
	store ChunkQualityReader
}

// NewChunkQualityHandler validates inputs.
func NewChunkQualityHandler(store ChunkQualityReader) (*ChunkQualityHandler, error) {
	if store == nil {
		return nil, errors.New("chunk_quality: nil store")
	}
	return &ChunkQualityHandler{store: store}, nil
}

// Register mounts GET /v1/admin/chunks/quality-report.
func (h *ChunkQualityHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/chunks/quality-report", h.report)
}

func (h *ChunkQualityHandler) report(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	rows, err := h.store.ListByTenant(c.Request.Context(), tenantID, 10000)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	buckets := map[string]*SourceBucket{}
	for _, r := range rows {
		b, ok := buckets[r.SourceID]
		if !ok {
			b = &SourceBucket{SourceID: r.SourceID}
			buckets[r.SourceID] = b
		}
		b.Count++
		b.AvgScore += r.QualityScore
		if r.QualityScore < 0.5 {
			b.BelowHalf++
		}
		if r.Duplicate {
			b.Duplicate++
		}
	}
	out := ChunkQualityReport{TenantID: tenantID, Buckets: make([]SourceBucket, 0, len(buckets))}
	for _, b := range buckets {
		if b.Count > 0 {
			b.AvgScore /= float64(b.Count)
		}
		out.Buckets = append(out.Buckets, *b)
	}
	sort.Slice(out.Buckets, func(i, j int) bool { return out.Buckets[i].SourceID < out.Buckets[j].SourceID })
	c.JSON(http.StatusOK, out)
}

// InMemoryChunkQualityStore is the test fake. Writers should
// populate via Insert; the handler reads via ListByTenant.
type InMemoryChunkQualityStore struct {
	mu   sync.RWMutex
	rows []ChunkQualityRow
}

// NewInMemoryChunkQualityStore returns the fake.
func NewInMemoryChunkQualityStore() *InMemoryChunkQualityStore {
	return &InMemoryChunkQualityStore{}
}

// Insert appends a row.
func (s *InMemoryChunkQualityStore) Insert(_ context.Context, row ChunkQualityRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
}

// ListByTenant returns a copy of all rows for tenantID.
func (s *InMemoryChunkQualityStore) ListByTenant(_ context.Context, tenantID string, _ int) ([]ChunkQualityRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ChunkQualityRow, 0, len(s.rows))
	for _, r := range s.rows {
		if r.TenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}
