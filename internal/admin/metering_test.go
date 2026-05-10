package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func newSQLiteMeteringStore(t *testing.T) (*admin.MeteringStoreGORM, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	store := admin.NewMeteringStoreGORM(db)
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store, db
}

func TestMetering_IncrementUpserts(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteMeteringStore(t)
	ctx := context.Background()
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := store.Increment(ctx, "tenant-a", day, admin.MetricNames.APIRetrieve, 5); err != nil {
			t.Fatalf("inc: %v", err)
		}
	}
	rows, err := store.List(ctx, "tenant-a", day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Count != 15 {
		t.Fatalf("count = %d, want 15", rows[0].Count)
	}
}

func TestMetering_TenantIsolation(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteMeteringStore(t)
	ctx := context.Background()
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	_ = store.Increment(ctx, "tenant-a", day, "x", 1)
	_ = store.Increment(ctx, "tenant-b", day, "x", 100)
	rows, _ := store.List(ctx, "tenant-a", day, day.Add(24*time.Hour))
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("tenant-a rows = %+v", rows)
	}
}

func TestMetering_CounterFlushBuffersIncrements(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteMeteringStore(t)
	c := admin.NewCounter(store)
	for i := 0; i < 10; i++ {
		c.Inc("tenant-a", admin.MetricNames.APIAdmin, 1)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	rows, _ := store.List(context.Background(), "tenant-a", day, day.Add(24*time.Hour))
	if len(rows) != 1 || rows[0].Count != 10 {
		t.Fatalf("rows = %+v", rows)
	}
}

// failingMeteringStore wraps a real store and forces Increment to
// error while fail is true. Used by the regression test below to
// simulate a transient DB blip during Counter.Flush().
type failingMeteringStore struct {
	inner admin.MeteringStore
	fail  bool
}

func (f *failingMeteringStore) Increment(ctx context.Context, tenantID string, day time.Time, metric string, delta int64) error {
	if f.fail {
		return errSimulatedDBFailure
	}
	return f.inner.Increment(ctx, tenantID, day, metric, delta)
}

func (f *failingMeteringStore) List(ctx context.Context, tenantID string, from, to time.Time) ([]admin.TenantUsage, error) {
	return f.inner.List(ctx, tenantID, from, to)
}

var errSimulatedDBFailure = stringError("simulated DB failure")

type stringError string

func (e stringError) Error() string { return string(e) }

// TestMetering_CounterFlushPreservesDeltasOnError is the
// regression for metering.go's Counter.Flush() swapping out the
// in-memory buffer before attempting DB writes — a Increment()
// failure used to permanently drop the delta because (a) the
// buffer had already been cleared and (b) FlushOnInterval ignores
// the returned error. For a billing/capacity counter that turned
// a transient DB blip into silent usage-data loss. The fix
// re-merges failed (key, delta) pairs back into c.buf so the next
// Flush retries them; we exercise that by failing the first flush
// and asserting the second flush lands the original deltas.
func TestMetering_CounterFlushPreservesDeltasOnError(t *testing.T) {
	t.Parallel()
	realStore, _ := newSQLiteMeteringStore(t)
	fake := &failingMeteringStore{inner: realStore, fail: true}
	c := admin.NewCounter(fake)

	for i := 0; i < 7; i++ {
		c.Inc("tenant-a", admin.MetricNames.APIRetrieve, 3)
	}
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("Flush should have surfaced the simulated DB failure")
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	rows, _ := realStore.List(context.Background(), "tenant-a", day, day.Add(24*time.Hour))
	if len(rows) != 0 {
		t.Fatalf("first flush failed but rows leaked into the store: %+v", rows)
	}

	fake.fail = false
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("second flush after recovery: %v", err)
	}
	rows, _ = realStore.List(context.Background(), "tenant-a", day, day.Add(24*time.Hour))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (deltas must be preserved across the failed flush)", len(rows))
	}
	if rows[0].Count != 21 {
		t.Fatalf("count = %d, want 21 (7 increments × 3); failed-flush deltas were dropped", rows[0].Count)
	}
}

func TestMetering_HTTP_HappyPath(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteMeteringStore(t)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	_ = store.Increment(ctx, "tenant-a", day, admin.MetricNames.APIRetrieve, 7)

	h := admin.NewMeteringHandler(store)
	g := gin.New()
	g.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Set(audit.ActorContextKey, "actor-1")
		c.Next()
	})
	rg := g.Group("/")
	h.Register(rg)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tenant-a/usage", nil)
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp admin.MeteringResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Days) == 0 {
		t.Fatalf("Days empty")
	}
	if resp.Days[0].Count != 7 {
		t.Fatalf("count = %d, want 7", resp.Days[0].Count)
	}
}

func TestMetering_HTTP_CrossTenantForbidden(t *testing.T) {
	t.Parallel()
	store, _ := newSQLiteMeteringStore(t)
	h := admin.NewMeteringHandler(store)
	g := gin.New()
	g.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	rg := g.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tenant-b/usage", nil)
	g.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}
