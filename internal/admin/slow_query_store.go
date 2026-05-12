// slow_query_store.go — Round-14 Task 3.
//
// Persistent slow-query log. Round-13 Task 8 added a `slow`
// boolean to the existing query_analytics table so operators
// could list recently-slow retrievals. Round-14 Task 3
// separates that detail into a dedicated `slow_queries` table
// (migrations/038_slow_queries.sql) so retention, indexing, and
// schema evolution stay decoupled from the analytics rollup.
//
// The retrieval handler invokes RecordSlowQuery() on every
// retrieval whose end-to-end latency crosses the slow-query
// threshold; the new /v1/admin/retrieval/slow-queries endpoint
// reads rows back ordered newest-first.
package admin

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// SlowQueryRow is the persisted shape; GORM tags map to columns
// in migrations/038_slow_queries.sql.
type SlowQueryRow struct {
	ID             string    `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID       string    `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	QueryHash      string    `gorm:"type:varchar(64);not null;column:query_hash" json:"query_hash"`
	QueryText      string    `gorm:"type:text;not null;column:query_text" json:"query_text"`
	LatencyMS      int       `gorm:"not null;column:latency_ms" json:"latency_ms"`
	TopK           int       `gorm:"not null;default:0;column:top_k" json:"top_k"`
	HitCount       int       `gorm:"not null;default:0;column:hit_count" json:"hit_count"`
	BackendTimings JSONMap   `gorm:"type:jsonb;column:backend_timings" json:"backend_timings,omitempty"`
	CreatedAt      time.Time `gorm:"not null;default:now();column:created_at" json:"created_at"`
}

// TableName pins the table name.
func (SlowQueryRow) TableName() string { return "slow_queries" }

// SlowQueryRecorder is the narrow write seam used by the
// retrieval handler. The implementation must degrade fail-open:
// a persistence failure must never propagate to the user
// request.
type SlowQueryRecorder interface {
	RecordSlowQuery(ctx context.Context, row *SlowQueryRow) error
}

// SlowQueryLister is the read seam used by the admin handler.
type SlowQueryLister interface {
	ListSlowQueries(ctx context.Context, q SlowQueryFilter) ([]*SlowQueryRow, error)
}

// SlowQueryStore unions the read and write seams.
type SlowQueryStore interface {
	SlowQueryRecorder
	SlowQueryLister
}

// SlowQueryFilter scopes a list query. TenantID is required.
type SlowQueryFilter struct {
	TenantID string
	Since    time.Time
	Limit    int
}

// SlowQueryStoreGORM is the Postgres-backed store.
type SlowQueryStoreGORM struct{ db *gorm.DB }

// NewSlowQueryStoreGORM constructs the store.
func NewSlowQueryStoreGORM(db *gorm.DB) *SlowQueryStoreGORM {
	return &SlowQueryStoreGORM{db: db}
}

// AutoMigrate runs the GORM migration (tests).
func (s *SlowQueryStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&SlowQueryRow{})
}

// RecordSlowQuery persists a row. A missing ID is auto-generated.
func (s *SlowQueryStoreGORM) RecordSlowQuery(ctx context.Context, row *SlowQueryRow) error {
	if row == nil {
		return errors.New("slow_queries: nil row")
	}
	if row.TenantID == "" {
		return errors.New("slow_queries: missing tenant_id")
	}
	if row.ID == "" {
		row.ID = ulid.Make().String()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Create(row).Error
}

// ListSlowQueries returns rows newest-first within the filter.
func (s *SlowQueryStoreGORM) ListSlowQueries(ctx context.Context, q SlowQueryFilter) ([]*SlowQueryRow, error) {
	if q.TenantID == "" {
		return nil, errors.New("slow_queries: missing tenant_id")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []*SlowQueryRow{}
	tx := s.db.WithContext(ctx).Model(&SlowQueryRow{}).Where("tenant_id = ?", q.TenantID)
	if !q.Since.IsZero() {
		tx = tx.Where("created_at >= ?", q.Since)
	}
	if err := tx.Order("created_at DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// InMemorySlowQueryStore is a goroutine-safe implementation for
// tests. The retrieval handler can also wire it as a degraded
// in-process fallback when the Postgres write fails.
type InMemorySlowQueryStore struct {
	mu   sync.Mutex
	rows []*SlowQueryRow
}

// NewInMemorySlowQueryStore returns an empty store.
func NewInMemorySlowQueryStore() *InMemorySlowQueryStore {
	return &InMemorySlowQueryStore{}
}

// RecordSlowQuery copies row and appends it to the in-memory
// list.
func (s *InMemorySlowQueryStore) RecordSlowQuery(_ context.Context, row *SlowQueryRow) error {
	if row == nil {
		return errors.New("slow_queries: nil row")
	}
	if row.TenantID == "" {
		return errors.New("slow_queries: missing tenant_id")
	}
	cp := *row
	if cp.ID == "" {
		cp.ID = ulid.Make().String()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.rows = append(s.rows, &cp)
	s.mu.Unlock()
	return nil
}

// ListSlowQueries returns matching rows newest-first.
func (s *InMemorySlowQueryStore) ListSlowQueries(_ context.Context, q SlowQueryFilter) ([]*SlowQueryRow, error) {
	if q.TenantID == "" {
		return nil, errors.New("slow_queries: missing tenant_id")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SlowQueryRow, 0, len(s.rows))
	for _, r := range s.rows {
		if r.TenantID != q.TenantID {
			continue
		}
		if !q.Since.IsZero() && r.CreatedAt.Before(q.Since) {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
