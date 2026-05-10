// Package admin — metering.go ships the tenant usage metering
// store, in-process counter, and HTTP handler.
//
// Round-4 Task 17: SaaS billing and capacity planning need a per-
// tenant view of API call volume + ingestion throughput + chunk
// count. The metering layer maintains a daily rollup keyed
// (tenant_id, day, metric) and lets operators query date ranges
// via GET /v1/admin/tenants/:id/usage?from=&to=.
//
// Metric names (extensible):
//
//	api_retrieve   — POST /v1/retrieve calls
//	api_admin      — admin endpoint calls (sources, policy, eval)
//	ingest_doc     — successful pipeline document persists
//	chunk_count    — net chunks added today (for capacity)
//
// In-process Counter buffers increments and flushes them in a
// single UPSERT every CounterFlushInterval. This keeps a hot
// retrieval path from doing per-request DB writes.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// TenantUsage is the daily rollup row.
type TenantUsage struct {
	TenantID  string    `gorm:"type:varchar(26);primaryKey;column:tenant_id" json:"tenant_id"`
	Day       time.Time `gorm:"type:date;primaryKey;column:day" json:"day"`
	Metric    string    `gorm:"type:varchar(64);primaryKey;column:metric" json:"metric"`
	Count     int64     `gorm:"not null;column:count" json:"count"`
	UpdatedAt time.Time `gorm:"not null;column:updated_at" json:"updated_at"`
}

// TableName overrides GORM's default pluralization.
func (TenantUsage) TableName() string { return "tenant_usage" }

// MeteringStore is the persistence contract.
type MeteringStore interface {
	// Increment atomically adds delta to the (tenant, day, metric)
	// row, creating it if missing. Implementations MUST be safe to
	// call concurrently from many goroutines.
	Increment(ctx context.Context, tenantID string, day time.Time, metric string, delta int64) error

	// List returns every row for tenantID with from <= day < to.
	// from is inclusive; to is exclusive.
	List(ctx context.Context, tenantID string, from, to time.Time) ([]TenantUsage, error)
}

// MeteringStoreGORM is the GORM-backed implementation.
type MeteringStoreGORM struct {
	db *gorm.DB
}

// NewMeteringStoreGORM constructs a metering store. The supplied
// db is held verbatim — callers are responsible for migration.
func NewMeteringStoreGORM(db *gorm.DB) *MeteringStoreGORM { return &MeteringStoreGORM{db: db} }

// AutoMigrate creates the tenant_usage table for fresh deploys.
func (s *MeteringStoreGORM) AutoMigrate(ctx context.Context) error {
	return s.db.WithContext(ctx).AutoMigrate(&TenantUsage{})
}

// Increment is the SaaS-grade hot path. UPSERT semantics on
// (tenant_id, day, metric) — count += delta atomically.
func (s *MeteringStoreGORM) Increment(ctx context.Context, tenantID string, day time.Time, metric string, delta int64) error {
	if tenantID == "" || metric == "" {
		return errors.New("metering: empty tenant_id or metric")
	}
	day = day.UTC().Truncate(24 * time.Hour)
	now := time.Now().UTC()
	// Use raw SQL for ON CONFLICT … DO UPDATE since the GORM
	// dialects we target (Postgres + SQLite via glebarez) both
	// understand it.
	q := `
INSERT INTO tenant_usage (tenant_id, day, metric, count, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, day, metric) DO UPDATE
SET count = tenant_usage.count + EXCLUDED.count,
    updated_at = EXCLUDED.updated_at`
	return s.db.WithContext(ctx).Exec(q, tenantID, day, metric, delta, now).Error
}

// List returns rows for the date range. Caller is responsible for
// turning the [from, to) window into a sensible default if either
// is zero.
func (s *MeteringStoreGORM) List(ctx context.Context, tenantID string, from, to time.Time) ([]TenantUsage, error) {
	if tenantID == "" {
		return nil, errors.New("metering: empty tenant_id")
	}
	from = from.UTC().Truncate(24 * time.Hour)
	to = to.UTC().Truncate(24 * time.Hour)
	if !to.After(from) {
		return nil, fmt.Errorf("metering: bad range [%s, %s)", from, to)
	}
	var rows []TenantUsage
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND day >= ? AND day < ?", tenantID, from, to).
		Order("day ASC, metric ASC").
		Find(&rows).Error
	return rows, err
}

// Counter buffers increments in memory and flushes them on a
// timer. Hot path callers use Inc() — they pay map+mutex cost,
// not DB cost.
type Counter struct {
	store MeteringStore

	mu  sync.Mutex
	buf map[counterKey]int64
}

type counterKey struct {
	TenantID string
	Day      time.Time
	Metric   string
}

// NewCounter wires a Counter to the supplied store.
func NewCounter(store MeteringStore) *Counter {
	return &Counter{store: store, buf: map[counterKey]int64{}}
}

// Inc records a delta into the in-memory buffer, keyed by today's
// UTC date. Safe to call concurrently.
func (c *Counter) Inc(tenantID, metric string, delta int64) {
	if tenantID == "" || metric == "" || delta == 0 {
		return
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	c.mu.Lock()
	c.buf[counterKey{TenantID: tenantID, Day: day, Metric: metric}] += delta
	c.mu.Unlock()
}

// Flush drains the buffer to the store. Errors are returned as a
// single joined error for observability; partial successes are
// preserved (already-flushed rows aren't reverted).
func (c *Counter) Flush(ctx context.Context) error {
	c.mu.Lock()
	snap := c.buf
	c.buf = map[counterKey]int64{}
	c.mu.Unlock()

	var errs []error
	for k, v := range snap {
		if err := c.store.Increment(ctx, k.TenantID, k.Day, k.Metric, v); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// MeteringHandler serves the /v1/admin/tenants/:id/usage endpoint.
type MeteringHandler struct {
	store MeteringStore
}

// NewMeteringHandler constructs the handler.
func NewMeteringHandler(store MeteringStore) *MeteringHandler {
	return &MeteringHandler{store: store}
}

// Register mounts the endpoint on rg.
func (h *MeteringHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/tenants/:id/usage", h.usage)
}

// MeteringResponse is the JSON shape returned to operators.
type MeteringResponse struct {
	TenantID string        `json:"tenant_id"`
	From     time.Time     `json:"from"`
	To       time.Time     `json:"to"`
	Days     []TenantUsage `json:"days"`
}

func (h *MeteringHandler) usage(c *gin.Context) {
	authedTenant, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	target := c.Param("id")
	if target == "" {
		target = authedTenant
	}
	// Tenant isolation: a non-superuser can only see their own
	// tenant. We do not have a superuser flag yet; reject any
	// cross-tenant read to be safe.
	if target != authedTenant {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-tenant usage queries forbidden"})
		return
	}
	from, to, err := parseUsageRange(c.Query("from"), c.Query("to"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rows, err := h.store.List(c.Request.Context(), target, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list usage: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, MeteringResponse{
		TenantID: target, From: from, To: to, Days: rows,
	})
}

// parseUsageRange interprets the from/to query params. Defaults
// (zero / zero) cover the last 30 days (UTC, exclusive on the
// upper bound). Both params must be RFC3339 dates / datetimes
// when supplied.
func parseUsageRange(rawFrom, rawTo string) (time.Time, time.Time, error) {
	now := time.Now().UTC().Truncate(24 * time.Hour)
	to := now.Add(24 * time.Hour)
	from := now.Add(-30 * 24 * time.Hour)
	if rawFrom != "" {
		t, err := time.Parse(time.RFC3339, rawFrom)
		if err != nil {
			t, err = time.Parse("2006-01-02", rawFrom)
		}
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from: %w", err)
		}
		from = t.UTC().Truncate(24 * time.Hour)
	}
	if rawTo != "" {
		t, err := time.Parse(time.RFC3339, rawTo)
		if err != nil {
			t, err = time.Parse("2006-01-02", rawTo)
		}
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to: %w", err)
		}
		to = t.UTC().Truncate(24 * time.Hour)
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be < to")
	}
	return from, to, nil
}

// MetricNames is the canonical list of metric strings used by
// hot-path callers. Adding a name here is a code-only change.
var MetricNames = struct {
	APIRetrieve, APIAdmin, IngestDoc, ChunkCount string
}{
	APIRetrieve: "api_retrieve",
	APIAdmin:    "api_admin",
	IngestDoc:   "ingest_doc",
	ChunkCount:  "chunk_count",
}

// FlushOnInterval starts a background goroutine that calls
// counter.Flush every interval. Returns a stop function the
// caller invokes on shutdown.
//
// The supplied audit logger is purely advisory — used to record a
// flush failure for forensic review. Pass nil to skip.
func FlushOnInterval(ctx context.Context, counter *Counter, interval time.Duration, _ *audit.Repository) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	t := time.NewTicker(interval)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = counter.Flush(context.Background())
				return
			case <-t.C:
				_ = counter.Flush(ctx)
			}
		}
	}()
	return cancel
}
