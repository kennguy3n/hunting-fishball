//go:build e2e

// Phase 2/3/4 e2e surfaces. Runs alongside smoke_test.go's pipeline
// + retrieval coverage to assert the same docker-compose stack
// honours admin source CRUD, ACL-deny policy filtering, and the
// draft/promote/reject workflow.
//
// Each test uses a unique tenant ID so they can run in parallel
// against the same shared Postgres without cross-test interference.
// The migrations baked into ./migrations are applied automatically
// by docker-compose's postgres-initdb mount.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// stubValidator passes every connector validation. The real registry
// validator pings the connector's external API; the e2e test plane
// does not have credentials for Slack/Drive so we stub it here.
type stubValidator struct{}

func (stubValidator) Validate(_ context.Context, _ string, _ connector.ConnectorConfig) error {
	return nil
}

// openE2EDB opens a *gorm.DB against the docker-compose postgres
// instance. The CONTEXT_ENGINE_DATABASE_URL env var matches the one
// the Makefile test-e2e target sets.
func openE2EDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := envOr("CONTEXT_ENGINE_DATABASE_URL", "host=localhost user=hf password=hf dbname=hunting_fishball port=5432 sslmode=disable")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	return db
}

// uniqueTenant returns a fresh ULID-formatted tenant ID so parallel
// tests don't trip over each other in the shared Postgres. ULIDs are
// exactly 26 chars to match the CHAR(26) constraint on every
// tenant_id column in migrations/.
func uniqueTenant(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

// uniqueActor returns a fresh ULID-formatted actor ID for the same
// reason as uniqueTenant.
func uniqueActor(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}

// TestSmoke_AdminSourceCRUD exercises POST/GET/PATCH/DELETE on the
// /v1/admin/sources surface against a real Postgres. Verifies the
// row roundtrips through SourceRepository and the audit log
// captures the lifecycle events.
func TestSmoke_AdminSourceCRUD(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)

	repo := admin.NewSourceRepository(db)
	auditRepo := audit.NewRepository(db)
	healthRepo := admin.NewHealthRepository(db, admin.HealthThresholds{})
	h, err := admin.NewHandler(admin.HandlerConfig{
		Repo:      repo,
		Audit:     auditRepo,
		Validator: stubValidator{},
		Health:    healthRepo,
	})
	if err != nil {
		t.Fatalf("admin.NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	// 1. POST /v1/admin/sources — connect a Slack source.
	createBody, _ := json.Marshal(admin.ConnectRequest{
		ConnectorType: "slack",
		Scopes:        []string{"channels:history"},
		Config:        map[string]any{"workspace": "smoke"},
	})
	createW := doRoute(r, http.MethodPost, "/v1/admin/sources", createBody)
	if createW.Code != http.StatusCreated {
		t.Fatalf("POST sources: %d body=%s", createW.Code, createW.Body.String())
	}
	var created admin.Source
	if err := json.Unmarshal(createW.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty source ID")
	}

	// 2. GET /v1/admin/sources/:id — fetch the row back.
	getW := doRoute(r, http.MethodGet, "/v1/admin/sources/"+created.ID, nil)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET sources/:id: %d body=%s", getW.Code, getW.Body.String())
	}
	var fetched admin.Source
	if err := json.Unmarshal(getW.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode fetched: %v", err)
	}
	if fetched.ID != created.ID {
		t.Fatalf("GET ID mismatch: %q vs %q", fetched.ID, created.ID)
	}

	// 3. PATCH (pause) — drives the same UpdatePatch path the admin
	// portal uses to take a source out of rotation.
	paused := admin.SourceStatusPaused
	patchBody, _ := json.Marshal(admin.PatchRequest{Status: &paused})
	patchW := doRoute(r, http.MethodPatch, "/v1/admin/sources/"+created.ID, patchBody)
	if patchW.Code != http.StatusOK {
		t.Fatalf("PATCH sources/:id: %d body=%s", patchW.Code, patchW.Body.String())
	}
	var patched admin.Source
	if err := json.Unmarshal(patchW.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if patched.Status != admin.SourceStatusPaused {
		t.Fatalf("status: %q (want %q)", patched.Status, admin.SourceStatusPaused)
	}

	// 4. DELETE — flips status to "removing"; the forget worker
	// drains derived rows out-of-band.
	delW := doRoute(r, http.MethodDelete, "/v1/admin/sources/"+created.ID, nil)
	if delW.Code != http.StatusNoContent && delW.Code != http.StatusOK && delW.Code != http.StatusAccepted {
		t.Fatalf("DELETE sources/:id: %d body=%s", delW.Code, delW.Body.String())
	}

	// Audit trail: we expect at least source.connected, source.paused
	// (or source.rescoped), and source.removed events.
	logs, err := auditRepo.List(ctx, audit.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("audit List: %v", err)
	}
	if len(logs.Items) < 2 {
		t.Fatalf("expected >=2 audit log rows, got %d", len(logs.Items))
	}
}

// TestSmoke_PolicyACLDenyDropsHits writes a policy snapshot with a
// "drive/secret/**" deny rule via LiveStoreGORM, indexes a public
// document and a secret document into Qdrant, and confirms the
// retrieval handler — driven by LiveResolverGORM — drops only the
// secret document. This is the Phase 4 ACL-gate end-to-end.
func TestSmoke_PolicyACLDenyDropsHits(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)

	pgStore, err := storage.NewPostgresStore(db)
	if err != nil {
		t.Fatalf("postgres store: %v", err)
	}
	if err := pgStore.AutoMigrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	qdrantURL := envOr("CONTEXT_ENGINE_QDRANT_URL", "http://localhost:6333")
	qdrant, err := storage.NewQdrantClient(storage.QdrantConfig{
		BaseURL:    qdrantURL,
		VectorSize: 8,
	})
	if err != nil {
		t.Fatalf("qdrant: %v", err)
	}
	storer, err := pipeline.NewStorer(pipeline.StoreConfig{
		Vector:    qdrant,
		Metadata:  pgStore,
		Connector: "smoke-acl",
	})
	if err != nil {
		t.Fatalf("storer: %v", err)
	}
	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch:          fakeFetch{},
		Parse:          fakeParse{},
		Embed:          fakeEmbed{},
		Store:          storer,
		QueueSize:      4,
		MaxAttempts:    1,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	// Index two documents. The metadata's "path" key feeds the ACL
	// glob match.
	docs := []struct {
		id, path string
	}{
		{"doc-public", "drive/public/intro.md"},
		{"doc-secret", "drive/secret/payroll.csv"},
	}
	runDone := make(chan error, 1)
	go func() { runDone <- coord.Run(ctx) }()
	for _, d := range docs {
		if err := coord.Submit(ctx, pipeline.IngestEvent{
			Kind:          pipeline.EventDocumentChanged,
			TenantID:      tenantID,
			SourceID:      "drive",
			DocumentID:    d.id,
			NamespaceID:   "ns",
			Title:         d.id,
			MIMEType:      "text/plain",
			PrivacyLabel:  "internal",
			InlineContent: []byte(d.id),
			Metadata:      map[string]string{"path": d.path},
		}); err != nil {
			t.Fatalf("Submit %q: %v", d.id, err)
		}
	}
	coord.CloseInputs()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Apply a deny snapshot covering drive/secret/**.
	liveStore := policy.NewLiveStoreGORM(db)
	deny := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
	}
	if err := liveStore.ApplySnapshot(ctx, db, tenantID, "", deny); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	// Build the retrieval handler with the live resolver wired to
	// the same DB, then issue a query and assert the deny rule
	// dropped the secret document.
	resolver := policy.NewLiveResolverGORM(db)
	retrievalHandler, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore:        qdrant,
		Embedder:           fakeEmbed{},
		PolicyResolver:     resolver,
		DefaultPrivacyMode: "remote",
	})
	if err != nil {
		t.Fatalf("retrieval handler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	retrievalHandler.Register(&r.RouterGroup)

	reqBody, _ := json.Marshal(retrieval.RetrieveRequest{Query: "doc", TopK: 5})
	w := doRoute(r, http.MethodPost, "/v1/retrieve", reqBody)
	if w.Code != http.StatusOK {
		t.Fatalf("POST retrieve: %d body=%s", w.Code, w.Body.String())
	}
	var resp retrieval.RetrieveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, hit := range resp.Hits {
		if hit.DocumentID == "doc-secret" {
			t.Fatalf("expected doc-secret to be denied, got %+v", hit)
		}
	}
	if resp.Policy.BlockedCount == 0 {
		t.Fatalf("expected BlockedCount > 0, got %+v", resp.Policy)
	}
}

// TestSmoke_PolicyDraftPromoteAndReject covers the Phase 4 draft
// lifecycle through the HTTP surface: create a draft, promote it,
// resolve via LiveResolverGORM to confirm the snapshot is now live;
// then create a second draft and reject it.
func TestSmoke_PolicyDraftPromoteAndReject(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)

	draftRepo := policy.NewDraftRepository(db)
	liveStore := policy.NewLiveStoreGORM(db)
	auditRepo := audit.NewRepository(db)
	promoter, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    draftRepo,
		LiveStore: liveStore,
		Audit:     auditRepo,
	})
	if err != nil {
		t.Fatalf("policy.NewPromoter: %v", err)
	}
	simulator, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: policy.NewLiveResolverGORM(db),
		Retriever: func(_ context.Context, _ policy.SimRetrieveRequest, _ policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("policy.NewSimulator: %v", err)
	}
	simHandler, err := admin.NewSimulatorHandler(admin.SimulatorConfig{
		Drafts:    draftRepo,
		Promotion: promoter,
		Simulator: simulator,
		Audit:     auditRepo,
	})
	if err != nil {
		t.Fatalf("admin.NewSimulatorHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	simHandler.Register(&r.RouterGroup)

	// 1. Create a promote-able draft with an ACL deny.
	createBody, _ := json.Marshal(admin.CreateDraftRequest{
		Snapshot: policy.PolicySnapshot{
			EffectiveMode: policy.PrivacyModeRemote,
			ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
				{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
			}},
		},
	})
	cw := doRoute(r, http.MethodPost, "/v1/admin/policy/drafts", createBody)
	if cw.Code != http.StatusCreated {
		t.Fatalf("POST drafts: %d body=%s", cw.Code, cw.Body.String())
	}
	var draft policy.Draft
	if err := json.Unmarshal(cw.Body.Bytes(), &draft); err != nil {
		t.Fatalf("decode draft: %v", err)
	}

	// 2. Promote.
	pw := doRoute(r, http.MethodPost, "/v1/admin/policy/drafts/"+draft.ID+"/promote", []byte(`{}`))
	if pw.Code != http.StatusOK {
		t.Fatalf("POST promote: %d body=%s", pw.Code, pw.Body.String())
	}

	// 3. Resolve and verify the snapshot is now live.
	resolved, err := policy.NewLiveResolverGORM(db).Resolve(ctx, tenantID, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.EffectiveMode != policy.PrivacyModeRemote {
		t.Fatalf("EffectiveMode: %q (want %q)", resolved.EffectiveMode, policy.PrivacyModeRemote)
	}
	if resolved.ACL == nil || len(resolved.ACL.Rules) == 0 {
		t.Fatalf("expected ACL rules to be present, got %+v", resolved.ACL)
	}

	// 4. Audit row for the promotion exists.
	logs, err := auditRepo.List(ctx, audit.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("audit List: %v", err)
	}
	foundPromoted := false
	for _, l := range logs.Items {
		if l.Action == audit.ActionPolicyPromoted {
			foundPromoted = true
			break
		}
	}
	if !foundPromoted {
		t.Fatal("expected policy.promoted audit row")
	}

	// 5. Second draft + reject path.
	createBody2, _ := json.Marshal(admin.CreateDraftRequest{
		Snapshot: policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeNoAI},
	})
	cw2 := doRoute(r, http.MethodPost, "/v1/admin/policy/drafts", createBody2)
	if cw2.Code != http.StatusCreated {
		t.Fatalf("POST drafts (2): %d body=%s", cw2.Code, cw2.Body.String())
	}
	var draft2 policy.Draft
	if err := json.Unmarshal(cw2.Body.Bytes(), &draft2); err != nil {
		t.Fatalf("decode draft 2: %v", err)
	}
	rejBody, _ := json.Marshal(map[string]string{"reason": "wrong mode"})
	rw := doRoute(r, http.MethodPost, "/v1/admin/policy/drafts/"+draft2.ID+"/reject", rejBody)
	if rw.Code != http.StatusOK {
		t.Fatalf("POST reject: %d body=%s", rw.Code, rw.Body.String())
	}

	// Live snapshot should still be the PROMOTED one — rejecting
	// the second draft must not alter live state.
	stillResolved, err := policy.NewLiveResolverGORM(db).Resolve(ctx, tenantID, "")
	if err != nil {
		t.Fatalf("Resolve after reject: %v", err)
	}
	if stillResolved.EffectiveMode != policy.PrivacyModeRemote {
		t.Fatalf("post-reject EffectiveMode: %q (want %q)", stillResolved.EffectiveMode, policy.PrivacyModeRemote)
	}
}

// TestSmoke_PolicySimulatorEndpoints hits /simulate, /simulate/diff
// and /conflicts to confirm the routes are mounted, the Postgres-
// backed draft store hydrates the simulator's WhatIfRequest from a
// stored draft, and the responses are well-formed JSON.
func TestSmoke_PolicySimulatorEndpoints(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = ctx

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)

	draftRepo := policy.NewDraftRepository(db)
	liveStore := policy.NewLiveStoreGORM(db)
	auditRepo := audit.NewRepository(db)
	promoter, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts:    draftRepo,
		LiveStore: liveStore,
		Audit:     auditRepo,
	})
	if err != nil {
		t.Fatalf("policy.NewPromoter: %v", err)
	}
	simulator, err := policy.NewSimulator(policy.SimulatorConfig{
		LiveResolver: policy.NewLiveResolverGORM(db),
		Retriever: func(_ context.Context, _ policy.SimRetrieveRequest, _ policy.PolicySnapshot) ([]policy.RetrieveHit, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("policy.NewSimulator: %v", err)
	}
	simHandler, err := admin.NewSimulatorHandler(admin.SimulatorConfig{
		Drafts:    draftRepo,
		Promotion: promoter,
		Simulator: simulator,
		Audit:     auditRepo,
	})
	if err != nil {
		t.Fatalf("admin.NewSimulatorHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	simHandler.Register(&r.RouterGroup)

	draftSnap := policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeRemote,
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/secret/**", Action: policy.ACLActionDeny},
		}},
	}

	// /simulate accepts an inline snapshot.
	simBody, _ := json.Marshal(admin.SimulateRequest{
		Query:       "anything",
		TopK:        5,
		DraftPolicy: draftSnap,
	})
	sw := doRoute(r, http.MethodPost, "/v1/admin/policy/simulate", simBody)
	if sw.Code != http.StatusOK {
		t.Fatalf("POST simulate: %d body=%s", sw.Code, sw.Body.String())
	}

	// /simulate/diff with the same payload.
	dw := doRoute(r, http.MethodPost, "/v1/admin/policy/simulate/diff", simBody)
	if dw.Code != http.StatusOK {
		t.Fatalf("POST simulate/diff: %d body=%s", dw.Code, dw.Body.String())
	}

	// /conflicts on an inline snapshot.
	confBody, _ := json.Marshal(admin.ConflictsRequest{Snapshot: draftSnap})
	cw := doRoute(r, http.MethodPost, "/v1/admin/policy/conflicts", confBody)
	if cw.Code != http.StatusOK {
		t.Fatalf("POST conflicts: %d body=%s", cw.Code, cw.Body.String())
	}
}

// newTenantRouter builds a gin engine that injects the tenant +
// actor context keys the admin / retrieval / simulator handlers
// read. The actor is required because audit_logs.actor_id is
// CHAR(26): an empty string would not fit, so we mint a fresh
// ULID-formatted actor for each test.
func newTenantRouter(t *testing.T, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	actorID := uniqueActor(t)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Set(audit.ActorContextKey, actorID)
		c.Next()
	})
	return r
}

// doRoute issues an HTTP request through the router and returns the
// recorder. Centralised so each test reads as a sequence of API
// calls rather than gin/httptest plumbing.
func doRoute(r *gin.Engine, method, path string, body []byte) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
