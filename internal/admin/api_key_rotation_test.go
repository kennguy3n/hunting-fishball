package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func newAPIKeyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&admin.APIKeyRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// fakeAuditRecorder lets the rotation test assert that an
// api_key.rotated row was emitted without standing up the full
// audit Repository.
type fakeAuditRecorder struct {
	mu      sync.Mutex
	records []*audit.AuditLog
}

func (f *fakeAuditRecorder) Create(_ context.Context, l *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *l
	f.records = append(f.records, &cp)
	return nil
}

func TestAPIKeyRotation_GeneratesNewKey(t *testing.T) {
	t.Parallel()
	db := newAPIKeyDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	rec := &fakeAuditRecorder{}
	h, err := admin.NewAPIKeyRotationHandler(admin.APIKeyRotationHandlerConfig{Store: store, Audit: rec})
	if err != nil {
		t.Fatalf("NewAPIKeyRotationHandler: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t-a/rotate-api-key", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body admin.APIKeyRotationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.APIKey, "hf_") {
		t.Fatalf("api_key=%q expected hf_ prefix", body.APIKey)
	}
	if len(body.APIKey) < 60 {
		t.Fatalf("api_key length=%d", len(body.APIKey))
	}
	if body.KeyID == "" {
		t.Fatalf("missing key_id")
	}
	if body.GraceUntil.Before(body.CreatedAt) {
		t.Fatalf("grace_until must be after created_at")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.records) != 1 || rec.records[0].Action != audit.ActionAPIKeyRotated {
		t.Fatalf("expected one api_key.rotated audit row, got %+v", rec.records)
	}
}

func TestAPIKeyRotation_PreviousKeyEntersGrace(t *testing.T) {
	t.Parallel()
	db := newAPIKeyDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	// Pre-existing active key.
	pre := &admin.APIKeyRow{
		ID: "01H000000000000000000000A0", TenantID: "t-a",
		KeyHash: strings.Repeat("a", 64), Status: string(admin.APIKeyStatusActive),
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := store.Insert(context.Background(), pre); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h, _ := admin.NewAPIKeyRotationHandler(admin.APIKeyRotationHandlerConfig{Store: store})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t-a/rotate-api-key", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var rows []admin.APIKeyRow
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	var active, grace int
	for _, r := range rows {
		switch r.Status {
		case string(admin.APIKeyStatusActive):
			active++
		case string(admin.APIKeyStatusGrace):
			grace++
			if r.GraceUntil == nil {
				t.Errorf("grace row missing grace_until")
			}
			if r.DeactivatedAt == nil {
				t.Errorf("grace row missing deactivated_at")
			}
		}
	}
	if active != 1 {
		t.Errorf("expected exactly 1 active row, got %d", active)
	}
	if grace != 1 {
		t.Errorf("expected exactly 1 grace row, got %d", grace)
	}
}

func TestAPIKeyRotation_CrossTenantForbidden(t *testing.T) {
	t.Parallel()
	db := newAPIKeyDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	h, _ := admin.NewAPIKeyRotationHandler(admin.APIKeyRotationHandlerConfig{Store: store})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t-b/rotate-api-key", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIKeyRotation_RequiresStore(t *testing.T) {
	t.Parallel()
	_, err := admin.NewAPIKeyRotationHandler(admin.APIKeyRotationHandlerConfig{})
	if err == nil || !strings.Contains(err.Error(), "Store") {
		t.Fatalf("err=%v", err)
	}
}

func TestAPIKeyGracePeriod_DefaultsTo24h(t *testing.T) {
	// Run serially: this test reads env.
	t.Setenv("CONTEXT_ENGINE_API_KEY_GRACE_PERIOD", "")
	if got := admin.APIKeyGracePeriod(); got != 24*time.Hour {
		t.Fatalf("default=%v want 24h", got)
	}
}

func TestAPIKeyGracePeriod_RespectsEnv(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_API_KEY_GRACE_PERIOD", "1h")
	if got := admin.APIKeyGracePeriod(); got != time.Hour {
		t.Fatalf("got=%v want 1h", got)
	}
}

// TestAPIKeyStoreGORM_Rotate_RollsBackOnInsertFailure asserts the
// transactional contract directly: if the insert leg of Rotate
// fails the grace flip must roll back so the tenant keeps the
// previously active key intact rather than ending up with zero
// active keys.
//
// Bug #5 (Devin Review on PR #22): before Rotate existed, the
// handler chained MoveActiveToGrace + Insert as two independent
// commits — an Insert failure left the tenant locked out with
// every key flipped to `grace`.
func TestAPIKeyStoreGORM_Rotate_RollsBackOnInsertFailure(t *testing.T) {
	t.Parallel()
	db := newAPIKeyDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	ctx := context.Background()
	pre := &admin.APIKeyRow{
		ID: "01H000000000000000000000A0", TenantID: "t-a",
		KeyHash: strings.Repeat("a", 64), Status: string(admin.APIKeyStatusActive),
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := store.Insert(ctx, pre); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Try to insert a duplicate row (same KeyHash) inside Rotate.
	// The unique index on key_hash rejects the Create call, which
	// must roll back the grace flip on the seeded row.
	dup := &admin.APIKeyRow{
		ID: "01H000000000000000000000B0", TenantID: "t-a",
		KeyHash:   pre.KeyHash, // collides with seeded row
		Status:    string(admin.APIKeyStatusActive),
		CreatedAt: time.Now().UTC(),
	}
	err := store.Rotate(ctx, "t-a", time.Now().UTC().Add(24*time.Hour), dup)
	if err == nil {
		t.Fatalf("expected Rotate to fail on duplicate key_hash")
	}
	var rows []admin.APIKeyRow
	if err := db.Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row after rollback, got %d", len(rows))
	}
	if rows[0].Status != string(admin.APIKeyStatusActive) {
		t.Errorf("seeded row status=%s want active (grace flip should have rolled back)", rows[0].Status)
	}
	if rows[0].DeactivatedAt != nil {
		t.Errorf("seeded row has deactivated_at=%v want nil", rows[0].DeactivatedAt)
	}
	if rows[0].GraceUntil != nil {
		t.Errorf("seeded row has grace_until=%v want nil", rows[0].GraceUntil)
	}
}

// TestAPIKeyStoreGORM_Rotate_HappyPath asserts that Rotate
// performs the grace flip and inserts the new active row when
// nothing fails. Pairs with the rollback test above.
func TestAPIKeyStoreGORM_Rotate_HappyPath(t *testing.T) {
	t.Parallel()
	db := newAPIKeyDB(t)
	store := admin.NewAPIKeyStoreGORM(db)
	ctx := context.Background()
	pre := &admin.APIKeyRow{
		ID: "01H000000000000000000000A0", TenantID: "t-a",
		KeyHash: strings.Repeat("a", 64), Status: string(admin.APIKeyStatusActive),
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := store.Insert(ctx, pre); err != nil {
		t.Fatalf("seed: %v", err)
	}
	newRow := &admin.APIKeyRow{
		ID: "01H000000000000000000000B0", TenantID: "t-a",
		KeyHash: strings.Repeat("b", 64), Status: string(admin.APIKeyStatusActive),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Rotate(ctx, "t-a", time.Now().UTC().Add(24*time.Hour), newRow); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	var rows []admin.APIKeyRow
	if err := db.Order("created_at ASC").Find(&rows).Error; err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows after rotate, got %d", len(rows))
	}
	if rows[0].Status != string(admin.APIKeyStatusGrace) {
		t.Errorf("seeded row status=%s want grace", rows[0].Status)
	}
	if rows[1].Status != string(admin.APIKeyStatusActive) {
		t.Errorf("new row status=%s want active", rows[1].Status)
	}
}
