// query_analytics.go — Round-7 Task 4.
//
// Per-retrieval query telemetry. The QueryAnalyticsRecorder is
// invoked at the end of every successful /v1/retrieve call and
// records (tenant_id, query_hash, query_text_truncated,
// backend_timings, hit_count, cache_hit, timestamp) into the
// query_analytics table (migrations/024_query_analytics.sql).
//
// The recorder is goroutine-safe and degrades fail-open: a failed
// insert is logged but never propagates back to the request path.
//
// The companion handler (query_analytics_handler.go) exposes the
// recorded events via GET /v1/admin/analytics/queries with time-
// range, tenant, and top-N filters. Tenant isolation is enforced
// at the SQL boundary (`tenant_id = ?` always present).
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// QueryAnalyticsRow is the persisted shape. GORM's column tags map
// to the migration in migrations/024_query_analytics.sql.
type QueryAnalyticsRow struct {
	ID             string    `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID       string    `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	QueryHash      string    `gorm:"type:varchar(64);not null;column:query_hash" json:"query_hash"`
	QueryText      string    `gorm:"type:text;not null;column:query_text" json:"query_text"`
	TopK           int       `gorm:"not null;column:top_k" json:"top_k"`
	HitCount       int       `gorm:"not null;column:hit_count" json:"hit_count"`
	CacheHit       bool      `gorm:"not null;column:cache_hit" json:"cache_hit"`
	LatencyMS      int       `gorm:"not null;column:latency_ms" json:"latency_ms"`
	BackendTimings JSONMap   `gorm:"type:jsonb;not null;default:'{}';column:backend_timings" json:"backend_timings"`
	ExperimentName string    `gorm:"type:varchar(64);column:experiment_name" json:"experiment_name,omitempty"`
	ExperimentArm  string    `gorm:"type:varchar(16);column:experiment_arm" json:"experiment_arm,omitempty"`
	CreatedAt      time.Time `gorm:"not null;default:now();column:created_at" json:"created_at"`
}

// TableName pins the table name; without this GORM would guess
// "query_analytics_rows".
func (QueryAnalyticsRow) TableName() string { return "query_analytics" }

// QueryAnalyticsEvent is the in-memory record-time payload.
// QueryText is truncated to 256 characters before persistence.
type QueryAnalyticsEvent struct {
	TenantID       string
	QueryText      string
	TopK           int
	HitCount       int
	CacheHit       bool
	LatencyMS      int
	BackendTimings map[string]int64
	ExperimentName string
	ExperimentArm  string
	At             time.Time
}

// QueryHash returns a stable sha256 prefix of the query text. Used
// for grouping repeated queries without persisting the full text.
func QueryHash(query string) string {
	sum := sha256.Sum256([]byte(query))
	return hex.EncodeToString(sum[:])
}

const queryTextTruncate = 256

// QueryAnalyticsStore is the persistence port. The production
// implementation is QueryAnalyticsStoreGORM; tests inject an
// in-memory fake.
type QueryAnalyticsStore interface {
	Record(ctx context.Context, row *QueryAnalyticsRow) error
	List(ctx context.Context, q QueryAnalyticsQuery) ([]*QueryAnalyticsRow, error)
	TopQueries(ctx context.Context, q QueryAnalyticsQuery) ([]TopQuery, error)
}

// QueryAnalyticsQuery filters the list/top endpoints.
type QueryAnalyticsQuery struct {
	TenantID string
	Since    time.Time
	Until    time.Time
	Limit    int
}

// TopQuery is the aggregate shape for the top-N projection.
type TopQuery struct {
	QueryHash    string  `json:"query_hash"`
	SampleText   string  `json:"sample_text"`
	Count        int     `json:"count"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	AvgHitCount  float64 `json:"avg_hit_count"`
	CacheHitPct  float64 `json:"cache_hit_pct"`
}

// QueryAnalyticsStoreGORM is the Postgres-backed store.
type QueryAnalyticsStoreGORM struct {
	db *gorm.DB
}

// NewQueryAnalyticsStoreGORM constructs the store.
func NewQueryAnalyticsStoreGORM(db *gorm.DB) *QueryAnalyticsStoreGORM {
	return &QueryAnalyticsStoreGORM{db: db}
}

// AutoMigrate runs the GORM migration for the table. Real
// deployments use migrations/024_query_analytics.sql instead.
func (s *QueryAnalyticsStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&QueryAnalyticsRow{})
}

// Record inserts a single analytics row.
func (s *QueryAnalyticsStoreGORM) Record(ctx context.Context, row *QueryAnalyticsRow) error {
	if row == nil {
		return errors.New("query_analytics: nil row")
	}
	if row.TenantID == "" {
		return errors.New("query_analytics: missing tenant_id")
	}
	if row.ID == "" {
		row.ID = ulid.Make().String()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.BackendTimings == nil {
		row.BackendTimings = JSONMap{}
	}
	return s.db.WithContext(ctx).Create(row).Error
}

// List returns the most recent rows within (Since, Until] for the
// tenant. tenant_id is always required.
func (s *QueryAnalyticsStoreGORM) List(ctx context.Context, q QueryAnalyticsQuery) ([]*QueryAnalyticsRow, error) {
	if q.TenantID == "" {
		return nil, errors.New("query_analytics: missing tenant_id")
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	out := []*QueryAnalyticsRow{}
	tx := s.db.WithContext(ctx).Model(&QueryAnalyticsRow{}).Where("tenant_id = ?", q.TenantID)
	if !q.Since.IsZero() {
		tx = tx.Where("created_at >= ?", q.Since)
	}
	if !q.Until.IsZero() {
		tx = tx.Where("created_at <= ?", q.Until)
	}
	if err := tx.Order("created_at DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// TopQueries returns the top-N most-frequently observed
// query_hashes ordered by count desc. Each entry includes a sample
// text + averaged backend timings / cache hit ratio.
func (s *QueryAnalyticsStoreGORM) TopQueries(ctx context.Context, q QueryAnalyticsQuery) ([]TopQuery, error) {
	rows, err := s.List(ctx, QueryAnalyticsQuery{TenantID: q.TenantID, Since: q.Since, Until: q.Until, Limit: 1000})
	if err != nil {
		return nil, err
	}
	return aggregateTopQueries(rows, q.Limit), nil
}

func aggregateTopQueries(rows []*QueryAnalyticsRow, limit int) []TopQuery {
	if limit <= 0 {
		limit = 20
	}
	grouped := map[string]*TopQuery{}
	totalLatency := map[string]int64{}
	totalHits := map[string]int64{}
	totalCacheHits := map[string]int{}
	for _, r := range rows {
		entry, ok := grouped[r.QueryHash]
		if !ok {
			entry = &TopQuery{QueryHash: r.QueryHash, SampleText: r.QueryText}
			grouped[r.QueryHash] = entry
		}
		entry.Count++
		totalLatency[r.QueryHash] += int64(r.LatencyMS)
		totalHits[r.QueryHash] += int64(r.HitCount)
		if r.CacheHit {
			totalCacheHits[r.QueryHash]++
		}
	}
	out := make([]TopQuery, 0, len(grouped))
	for h, e := range grouped {
		if e.Count > 0 {
			e.AvgLatencyMS = float64(totalLatency[h]) / float64(e.Count)
			e.AvgHitCount = float64(totalHits[h]) / float64(e.Count)
			e.CacheHitPct = float64(totalCacheHits[h]) / float64(e.Count) * 100.0
		}
		out = append(out, *e)
	}
	// Order by count desc, then query_hash asc for stability.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Count > out[i].Count || (out[j].Count == out[i].Count && out[j].QueryHash < out[i].QueryHash) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// InMemoryQueryAnalyticsStore is the test fake.
type InMemoryQueryAnalyticsStore struct {
	mu   sync.RWMutex
	rows []*QueryAnalyticsRow
}

// NewInMemoryQueryAnalyticsStore builds a fresh in-memory store.
func NewInMemoryQueryAnalyticsStore() *InMemoryQueryAnalyticsStore {
	return &InMemoryQueryAnalyticsStore{}
}

// Record stores row.
func (s *InMemoryQueryAnalyticsStore) Record(_ context.Context, row *QueryAnalyticsRow) error {
	if row == nil {
		return errors.New("query_analytics: nil row")
	}
	if row.TenantID == "" {
		return errors.New("query_analytics: missing tenant_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *row
	if cp.ID == "" {
		cp.ID = ulid.Make().String()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now().UTC()
	}
	s.rows = append(s.rows, &cp)
	return nil
}

// List returns rows for the tenant filtered by time-range.
func (s *InMemoryQueryAnalyticsStore) List(_ context.Context, q QueryAnalyticsQuery) ([]*QueryAnalyticsRow, error) {
	if q.TenantID == "" {
		return nil, errors.New("query_analytics: missing tenant_id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*QueryAnalyticsRow{}
	for _, r := range s.rows {
		if r.TenantID != q.TenantID {
			continue
		}
		if !q.Since.IsZero() && r.CreatedAt.Before(q.Since) {
			continue
		}
		if !q.Until.IsZero() && r.CreatedAt.After(q.Until) {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	// Order newest first.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// TopQueries delegates to aggregateTopQueries against in-memory
// rows.
func (s *InMemoryQueryAnalyticsStore) TopQueries(_ context.Context, q QueryAnalyticsQuery) ([]TopQuery, error) {
	rows, err := s.List(context.Background(), QueryAnalyticsQuery{TenantID: q.TenantID, Since: q.Since, Until: q.Until, Limit: 10000})
	if err != nil {
		return nil, err
	}
	return aggregateTopQueries(rows, q.Limit), nil
}

// QueryAnalyticsRecorder is the call-site facade. The retrieval
// handler invokes Record after writing a successful response; the
// recorder hashes the query, truncates the text, and persists the
// row via the configured store.
type QueryAnalyticsRecorder struct {
	store  QueryAnalyticsStore
	logger func(string, ...any)
}

// NewQueryAnalyticsRecorder validates inputs.
func NewQueryAnalyticsRecorder(store QueryAnalyticsStore) (*QueryAnalyticsRecorder, error) {
	if store == nil {
		return nil, errors.New("query_analytics: nil store")
	}
	return &QueryAnalyticsRecorder{store: store, logger: func(string, ...any) {}}, nil
}

// Record persists the event. Errors are logged and swallowed —
// retrieval must never fail because analytics couldn't be written.
func (r *QueryAnalyticsRecorder) Record(ctx context.Context, evt QueryAnalyticsEvent) {
	if r == nil || evt.TenantID == "" {
		return
	}
	text := evt.QueryText
	if len(text) > queryTextTruncate {
		text = text[:queryTextTruncate]
	}
	at := evt.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	timings := JSONMap{}
	for k, v := range evt.BackendTimings {
		timings[k] = v
	}
	// Smoke-test the JSON envelope so a misbehaving downstream
	// driver doesn't error mid-Insert.
	if _, err := json.Marshal(timings); err != nil {
		timings = JSONMap{}
	}
	row := &QueryAnalyticsRow{
		ID:             ulid.Make().String(),
		TenantID:       evt.TenantID,
		QueryHash:      QueryHash(evt.QueryText),
		QueryText:      text,
		TopK:           evt.TopK,
		HitCount:       evt.HitCount,
		CacheHit:       evt.CacheHit,
		LatencyMS:      evt.LatencyMS,
		BackendTimings: timings,
		ExperimentName: evt.ExperimentName,
		ExperimentArm:  evt.ExperimentArm,
		CreatedAt:      at,
	}
	if err := r.store.Record(ctx, row); err != nil {
		r.logger("query_analytics: record failed", "tenant_id", evt.TenantID, "error", err)
	}
}

// Store exposes the underlying store for callers that want to
// query the recorded data without re-wiring a separate connection.
func (r *QueryAnalyticsRecorder) Store() QueryAnalyticsStore { return r.store }
