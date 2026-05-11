//go:build e2e

// Package e2e — round9_test.go — Round-10 Task 11.
//
// Drives the five Round-9 GORM-backed admin surfaces end-to-end
// through their HTTP handlers and asserts tenant isolation on
// each:
//
//   - NotificationStore CRUD (NotificationHandler)
//   - ABTest CRUD + list (ABTestHandler)
//   - ConnectorTemplate create + get + delete (ConnectorTemplateHandler)
//   - SynonymStore CRUD (SynonymsHandler)
//   - ChunkQuality insert + list (ChunkQualityHandler)
//
// Each subtest spins up a private SQLite GORM session per store
// to keep the suite docker-compose-free. The handler surface is
// the production code path; only the storage substrate is swapped.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

const (
	round9TenantA = "tenant-r9-a"
	round9TenantB = "tenant-r9-b"
)

// round9Audit is the bare-bones audit recorder. Round-9 handlers
// emit audit rows on every mutation; the e2e harness doesn't care
// about content but must satisfy the interface so the handler
// doesn't NPE.
type round9Audit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (r *round9Audit) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}

func round9SQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	return db
}

// round9RouterFor wires a router that injects the supplied tenant
// into every request via the audit middleware context key.
func round9RouterFor(tenantID string, register func(*gin.RouterGroup)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Set(audit.ActorContextKey, "actor-r9")
		c.Next()
	})
	register(&r.RouterGroup)
	return r
}

func round9Do(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ----- T11.1: NotificationStore CRUD + tenant isolation -----

func TestRound9_Notifications_CRUDAndTenantIsolation(t *testing.T) {
	db := round9SQLite(t)
	store, err := admin.NewNotificationStoreGORM(db)
	if err != nil {
		t.Fatalf("NewNotificationStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := admin.NewNotificationHandler(store, &round9Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	body := admin.NotificationPreference{
		EventType: "source.created",
		Channel:   admin.NotificationChannelWebhook,
		Target:    "https://hooks.example/a",
	}
	rA := round9RouterFor(round9TenantA, h.Register)
	if w := round9Do(t, rA, http.MethodPost, "/v1/admin/notifications", body); w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("tenant-a create status: %d body=%s", w.Code, w.Body.String())
	}
	rB := round9RouterFor(round9TenantB, h.Register)
	bodyB := body
	bodyB.Target = "https://hooks.example/b"
	if w := round9Do(t, rB, http.MethodPost, "/v1/admin/notifications", bodyB); w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("tenant-b create status: %d body=%s", w.Code, w.Body.String())
	}

	w := round9Do(t, rA, http.MethodGet, "/v1/admin/notifications", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list tenant-a: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "hooks.example/b") {
		t.Fatalf("tenant isolation violated: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hooks.example/a") {
		t.Fatalf("tenant-a missing own row: %s", w.Body.String())
	}
}

// ----- T11.2: ABTest CRUD + list + tenant isolation -----

func TestRound9_ABTests_UpsertGetListDelete(t *testing.T) {
	db := round9SQLite(t)
	store, err := admin.NewABTestStoreGORM(db)
	if err != nil {
		t.Fatalf("ABTestStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := admin.NewABTestHandler(store, &round9Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	rA := round9RouterFor(round9TenantA, h.Register)
	rB := round9RouterFor(round9TenantB, h.Register)

	exp := admin.ABTestConfig{
		ExperimentName:      "topk-bump",
		Status:              "active",
		TrafficSplitPercent: 50,
		ControlConfig:       map[string]any{"top_k": 5},
		VariantConfig:       map[string]any{"top_k": 25},
	}
	if w := round9Do(t, rA, http.MethodPost, "/v1/admin/retrieval/experiments", exp); w.Code != http.StatusOK {
		t.Fatalf("upsert: %d body=%s", w.Code, w.Body.String())
	}
	if w := round9Do(t, rA, http.MethodGet, "/v1/admin/retrieval/experiments/topk-bump", nil); w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	// tenant-b must not see tenant-a's experiment.
	if w := round9Do(t, rB, http.MethodGet, "/v1/admin/retrieval/experiments/topk-bump", nil); w.Code == http.StatusOK {
		t.Fatalf("tenant isolation violated: tenant-b read tenant-a row")
	}
	if w := round9Do(t, rA, http.MethodGet, "/v1/admin/retrieval/experiments", nil); !strings.Contains(w.Body.String(), "topk-bump") {
		t.Fatalf("list missing experiment: %s", w.Body.String())
	}
	if w := round9Do(t, rA, http.MethodDelete, "/v1/admin/retrieval/experiments/topk-bump", nil); w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
}

// ----- T11.3: ConnectorTemplate create + get + delete -----

func TestRound9_ConnectorTemplates_CreateGetDelete(t *testing.T) {
	db := round9SQLite(t)
	store, err := admin.NewConnectorTemplateStoreGORM(db)
	if err != nil {
		t.Fatalf("ConnectorTemplateStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := admin.NewConnectorTemplateHandler(store, &round9Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	rA := round9RouterFor(round9TenantA, h.Register)
	rB := round9RouterFor(round9TenantB, h.Register)

	tmpl := admin.ConnectorTemplate{
		ConnectorType: "google-drive",
		DefaultConfig: admin.JSONMap{"folder_id": "root"},
		Description:   "default drive ingestion",
	}
	w := round9Do(t, rA, http.MethodPost, "/v1/admin/connector-templates", tmpl)
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var created admin.ConnectorTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created template missing ID")
	}
	if g := round9Do(t, rA, http.MethodGet, "/v1/admin/connector-templates/"+created.ID, nil); g.Code != http.StatusOK {
		t.Fatalf("get: %d", g.Code)
	}
	// tenant-b can't read tenant-a's template.
	if g := round9Do(t, rB, http.MethodGet, "/v1/admin/connector-templates/"+created.ID, nil); g.Code == http.StatusOK {
		t.Fatalf("tenant isolation violated: tenant-b read tenant-a template")
	}
	if d := round9Do(t, rA, http.MethodDelete, "/v1/admin/connector-templates/"+created.ID, nil); d.Code != http.StatusOK && d.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", d.Code)
	}
}

// ----- T11.4: SynonymStore CRUD + tenant isolation -----

func TestRound9_Synonyms_CRUDAndTenantIsolation(t *testing.T) {
	db := round9SQLite(t)
	store, err := retrieval.NewSynonymStoreGORM(db)
	if err != nil {
		t.Fatalf("NewSynonymStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h, err := admin.NewSynonymsHandler(store, &round9Audit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	rA := round9RouterFor(round9TenantA, h.Register)
	rB := round9RouterFor(round9TenantB, h.Register)

	if err := store.Set(context.Background(), round9TenantA, map[string][]string{"car": {"vehicle"}}); err != nil {
		t.Fatalf("seed tenant-a: %v", err)
	}
	if err := store.Set(context.Background(), round9TenantB, map[string][]string{"truck": {"lorry"}}); err != nil {
		t.Fatalf("seed tenant-b: %v", err)
	}

	w := round9Do(t, rA, http.MethodGet, "/v1/admin/synonyms", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list tenant-a: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vehicle") {
		t.Fatalf("tenant-a missing own synonyms: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "lorry") {
		t.Fatalf("tenant isolation violated: %s", w.Body.String())
	}
	if w := round9Do(t, rB, http.MethodGet, "/v1/admin/synonyms", nil); !strings.Contains(w.Body.String(), "lorry") {
		t.Fatalf("tenant-b list missing own synonyms: %s", w.Body.String())
	}
}

// ----- T11.5: ChunkQuality insert + list + tenant isolation -----

func TestRound9_ChunkQuality_InsertAndList(t *testing.T) {
	db := round9SQLite(t)
	store, err := admin.NewChunkQualityStoreGORM(db)
	if err != nil {
		t.Fatalf("NewChunkQualityStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Insert one row for tenant-a and one for tenant-b.
	if err := store.Insert(context.Background(), admin.ChunkQualityRow{
		TenantID: round9TenantA, SourceID: "s-a", DocumentID: "d-a", ChunkID: "c-a", QualityScore: 0.91,
	}); err != nil {
		t.Fatalf("insert tenant-a: %v", err)
	}
	if err := store.Insert(context.Background(), admin.ChunkQualityRow{
		TenantID: round9TenantB, SourceID: "s-b", DocumentID: "d-b", ChunkID: "c-b", QualityScore: 0.42,
	}); err != nil {
		t.Fatalf("insert tenant-b: %v", err)
	}
	h, err := admin.NewChunkQualityHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	rA := round9RouterFor(round9TenantA, h.Register)
	rB := round9RouterFor(round9TenantB, h.Register)

	wA := round9Do(t, rA, http.MethodGet, "/v1/admin/chunks/quality-report", nil)
	if wA.Code != http.StatusOK {
		t.Fatalf("list tenant-a: %d body=%s", wA.Code, wA.Body.String())
	}
	if strings.Contains(wA.Body.String(), "c-b") {
		t.Fatalf("tenant isolation violated: tenant-a saw tenant-b chunk")
	}

	wB := round9Do(t, rB, http.MethodGet, "/v1/admin/chunks/quality-report", nil)
	if wB.Code != http.StatusOK {
		t.Fatalf("list tenant-b: %d", wB.Code)
	}
	if strings.Contains(wB.Body.String(), "c-a") {
		t.Fatalf("tenant isolation violated: tenant-b saw tenant-a chunk")
	}
}
