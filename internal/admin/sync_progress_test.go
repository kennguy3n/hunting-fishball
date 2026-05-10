package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func newProgressDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&admin.SyncProgress{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func TestSyncProgress_IncrementInsertsThenUpdates(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()
	if err := store.IncrementDiscovered(ctx, "t-a", "s-1", "ns-1", 5); err != nil {
		t.Fatalf("disc: %v", err)
	}
	if err := store.IncrementProcessed(ctx, "t-a", "s-1", "ns-1", 2); err != nil {
		t.Fatalf("proc: %v", err)
	}
	if err := store.IncrementFailed(ctx, "t-a", "s-1", "ns-1", 1); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := store.IncrementDiscovered(ctx, "t-a", "s-1", "ns-1", 3); err != nil {
		t.Fatalf("disc2: %v", err)
	}
	rows, err := store.List(ctx, "t-a", "s-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Discovered != 8 || rows[0].Processed != 2 || rows[0].Failed != 1 {
		t.Fatalf("counters: %+v", rows[0])
	}
}

func TestSyncProgress_PerNamespace(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()
	_ = store.IncrementDiscovered(ctx, "t-a", "s-1", "a", 4)
	_ = store.IncrementDiscovered(ctx, "t-a", "s-1", "b", 9)
	rows, _ := store.List(ctx, "t-a", "s-1")
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
}

func TestSyncProgress_TenantIsolation(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()
	_ = store.IncrementDiscovered(ctx, "t-a", "s-1", "ns", 5)
	_ = store.IncrementDiscovered(ctx, "t-b", "s-1", "ns", 7)
	rows, _ := store.List(ctx, "t-a", "s-1")
	if len(rows) != 1 || rows[0].Discovered != 5 {
		t.Fatalf("tenant leak: %+v", rows)
	}
}

func TestSyncProgress_HandlerHappyPath(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()
	_ = store.IncrementDiscovered(ctx, "t-a", "s-1", "ns-1", 10)
	_ = store.IncrementProcessed(ctx, "t-a", "s-1", "ns-1", 7)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	admin.NewSyncProgressHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/s-1/progress", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var body admin.SyncProgressResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items=%+v", body.Items)
	}
	got := body.Items[0]
	if got.Discovered != 10 || got.Processed != 7 || got.PercentDone != 70 {
		t.Fatalf("got=%+v", got)
	}
}

func TestSyncProgress_HandlerRequiresTenant(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/")
	admin.NewSyncProgressHandler(store).Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/s-1/progress", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestSyncProgress_ConcurrentIncrement(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()
	var wg sync.WaitGroup
	const N = 20
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.IncrementProcessed(ctx, "t-a", "s-1", "ns-1", 1); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	// Post-fix (Devin Review on PR #12), the increment is a single
	// atomic INSERT ... ON CONFLICT DO UPDATE, so concurrent calls
	// can no longer hit a primary-key conflict and no updates are
	// lost. Pre-fix this test was lenient because the check-then-act
	// pattern allowed both races to slip through; the assertions are
	// now exact.
	for err := range errs {
		t.Fatalf("concurrent increment errored: %v", err)
	}
	rows, _ := store.List(ctx, "t-a", "s-1")
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Processed != N {
		t.Fatalf("expected exactly %d, got %d", N, rows[0].Processed)
	}
}

// TestSyncProgress_ConcurrentFirstInsert is the explicit regression
// test for the TOCTOU bug surfaced by Devin Review on PR #12. Pre-fix,
// the increment path was UPDATE-then-CREATE: both goroutines would see
// `RowsAffected==0` from the UPDATE on a row that doesn't yet exist,
// then both would attempt CREATE — and the second would hit a
// primary-key violation on Postgres. SQLite without WAL mode and with
// `MaxOpenConns(1)` already serialises writes, so the test focuses on
// the post-fix behaviour: every goroutine must succeed AND the final
// counter must equal the sum of every delta (no lost updates).
func TestSyncProgress_ConcurrentFirstInsert(t *testing.T) {
	t.Parallel()
	db := newProgressDB(t)
	store := admin.NewSyncProgressStoreGORM(db)
	ctx := context.Background()

	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // align goroutines so we maximise the race window
			if err := store.IncrementDiscovered(ctx, "t-a", "s-new", "ns-new", 1); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent first-insert errored: %v", err)
	}
	rows, _ := store.List(ctx, "t-a", "s-new")
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row after concurrent first-insert, got %d", len(rows))
	}
	if rows[0].Discovered != N {
		t.Fatalf("expected Discovered=%d (no lost updates), got %d", N, rows[0].Discovered)
	}
}
