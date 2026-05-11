// sync_history.go — Round-7 Task 16.
//
// SyncHistoryRecorder writes one row per sync run per (tenant,
// source). The pipeline consumer calls Start() at the beginning
// of a sync run and Finish() at the end with the doc counts; the
// recorder upserts the row so the admin portal can display a
// per-source history table.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SyncStatus is the lifecycle marker.
type SyncStatus string

const (
	SyncStatusRunning   SyncStatus = "running"
	SyncStatusSucceeded SyncStatus = "succeeded"
	SyncStatusFailed    SyncStatus = "failed"
)

// SyncHistoryRow is the persisted shape.
type SyncHistoryRow struct {
	TenantID      string     `gorm:"column:tenant_id;type:char(26);primaryKey" json:"tenant_id"`
	SourceID      string     `gorm:"column:source_id;type:char(26);primaryKey" json:"source_id"`
	SyncRunID     string     `gorm:"column:sync_run_id;primaryKey" json:"sync_run_id"`
	Status        SyncStatus `gorm:"column:status" json:"status"`
	DocsProcessed int        `gorm:"column:docs_processed" json:"docs_processed"`
	DocsFailed    int        `gorm:"column:docs_failed" json:"docs_failed"`
	StartedAt     time.Time  `gorm:"column:started_at" json:"started_at"`
	EndedAt       *time.Time `gorm:"column:ended_at" json:"ended_at,omitempty"`
	DurationMS    int64      `gorm:"column:duration_ms" json:"duration_ms"`
}

// TableName overrides GORM pluralization.
func (SyncHistoryRow) TableName() string { return "sync_history" }

// SyncHistoryRecorder writes sync rows.
type SyncHistoryRecorder interface {
	Start(ctx context.Context, tenantID, sourceID, runID string) error
	Finish(ctx context.Context, tenantID, sourceID, runID string, status SyncStatus, processed, failed int) error
	List(ctx context.Context, tenantID, sourceID string, limit int) ([]SyncHistoryRow, error)
}

// SyncHistoryGORM is the Postgres-backed recorder.
type SyncHistoryGORM struct {
	db  *gorm.DB
	now func() time.Time
}

// NewSyncHistoryGORM validates inputs.
func NewSyncHistoryGORM(db *gorm.DB) (*SyncHistoryGORM, error) {
	if db == nil {
		return nil, errors.New("sync_history: nil db")
	}
	return &SyncHistoryGORM{db: db, now: time.Now}, nil
}

// Start inserts a new row in `running` state.
func (s *SyncHistoryGORM) Start(ctx context.Context, tenantID, sourceID, runID string) error {
	row := SyncHistoryRow{
		TenantID: tenantID, SourceID: sourceID, SyncRunID: runID,
		Status: SyncStatusRunning, StartedAt: s.now().UTC(),
	}
	return s.db.WithContext(ctx).Create(&row).Error
}

// Finish updates an existing row with final status + counts.
func (s *SyncHistoryGORM) Finish(ctx context.Context, tenantID, sourceID, runID string, status SyncStatus, processed, failed int) error {
	now := s.now().UTC()
	updates := map[string]any{
		"status":         string(status),
		"docs_processed": processed,
		"docs_failed":    failed,
		"ended_at":       now,
	}
	// We can't compute duration_ms in a generic dialect — fetch
	// then update.
	var existing SyncHistoryRow
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ? AND sync_run_id = ?", tenantID, sourceID, runID).
		First(&existing).Error
	if err != nil {
		return err
	}
	updates["duration_ms"] = now.Sub(existing.StartedAt).Milliseconds()
	return s.db.WithContext(ctx).Model(&SyncHistoryRow{}).
		Where("tenant_id = ? AND source_id = ? AND sync_run_id = ?", tenantID, sourceID, runID).
		Updates(updates).Error
}

// List returns the most recent rows (paginated by limit).
func (s *SyncHistoryGORM) List(ctx context.Context, tenantID, sourceID string, limit int) ([]SyncHistoryRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []SyncHistoryRow
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		Order("started_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// InMemorySyncHistoryRecorder is the test fake.
type InMemorySyncHistoryRecorder struct {
	mu   sync.RWMutex
	rows []SyncHistoryRow
	now  func() time.Time
}

// NewInMemorySyncHistoryRecorder returns the fake. Override `now`
// for deterministic durations.
func NewInMemorySyncHistoryRecorder() *InMemorySyncHistoryRecorder {
	return &InMemorySyncHistoryRecorder{now: time.Now}
}

// Start inserts a row in `running` state.
func (s *InMemorySyncHistoryRecorder) Start(_ context.Context, tenantID, sourceID, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, SyncHistoryRow{
		TenantID: tenantID, SourceID: sourceID, SyncRunID: runID,
		Status: SyncStatusRunning, StartedAt: s.now().UTC(),
	})
	return nil
}

// Finish updates the matching row.
func (s *InMemorySyncHistoryRecorder) Finish(_ context.Context, tenantID, sourceID, runID string, status SyncStatus, processed, failed int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		r := &s.rows[i]
		if r.TenantID == tenantID && r.SourceID == sourceID && r.SyncRunID == runID {
			now := s.now().UTC()
			r.Status = status
			r.DocsProcessed = processed
			r.DocsFailed = failed
			r.EndedAt = &now
			r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
			return nil
		}
	}
	return errors.New("sync_history: run not found")
}

// List returns rows for (tenant, source), most recent first.
func (s *InMemorySyncHistoryRecorder) List(_ context.Context, tenantID, sourceID string, limit int) ([]SyncHistoryRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []SyncHistoryRow
	for _, r := range s.rows {
		if r.TenantID == tenantID && r.SourceID == sourceID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SyncHistoryHandler serves
// GET /v1/admin/sources/:id/sync-history.
type SyncHistoryHandler struct {
	rec SyncHistoryRecorder
}

// NewSyncHistoryHandler validates inputs.
func NewSyncHistoryHandler(rec SyncHistoryRecorder) (*SyncHistoryHandler, error) {
	if rec == nil {
		return nil, errors.New("sync_history: nil recorder")
	}
	return &SyncHistoryHandler{rec: rec}, nil
}

// Register mounts the route.
func (h *SyncHistoryHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/sources/:id/sync-history", h.list)
}

func (h *SyncHistoryHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	rows, err := h.rec.List(c.Request.Context(), tenantID, sourceID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}
