package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// fakeDraftStore is an in-memory DraftStore for the simulator
// handler tests. It mirrors the GORM repo's tenant isolation so the
// handler exercises the real cross-tenant denial path.
type fakeDraftStore struct {
	mu     sync.Mutex
	byID   map[string]*policy.Draft
	getErr error
}

func newFakeDraftStore() *fakeDraftStore {
	return &fakeDraftStore{byID: map[string]*policy.Draft{}}
}

func (f *fakeDraftStore) Create(_ context.Context, d *policy.Draft) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *d
	f.byID[d.ID] = &cp
	return nil
}

func (f *fakeDraftStore) Get(_ context.Context, tenantID, id string) (*policy.Draft, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	d, ok := f.byID[id]
	if !ok || d.TenantID != tenantID {
		return nil, policy.ErrDraftNotFound
	}
	cp := *d
	return &cp, nil
}

func (f *fakeDraftStore) List(_ context.Context, ff policy.DraftListFilter) ([]policy.Draft, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []policy.Draft{}
	for _, d := range f.byID {
		if d.TenantID != ff.TenantID {
			continue
		}
		if ff.Status != "" && d.Status != ff.Status {
			continue
		}
		out = append(out, *d)
	}
	return out, nil
}

// fakePromotion implements PromotionService for the handler tests.
type fakePromotion struct {
	mu          sync.Mutex
	promoteCall int
	rejectCall  int
	promoteErr  error
	rejectErr   error
}

func (f *fakePromotion) PromoteDraft(_ context.Context, tenantID, draftID, actorID string) (*policy.Draft, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoteCall++
	if f.promoteErr != nil {
		return nil, f.promoteErr
	}
	return &policy.Draft{ID: draftID, TenantID: tenantID, Status: policy.DraftStatusPromoted, PromotedBy: actorID}, nil
}

func (f *fakePromotion) RejectDraft(_ context.Context, tenantID, draftID, actorID, reason string) (*policy.Draft, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectCall++
	if f.rejectErr != nil {
		return nil, f.rejectErr
	}
	return &policy.Draft{ID: draftID, TenantID: tenantID, Status: policy.DraftStatusRejected, PromotedBy: actorID, RejectReason: reason}, nil
}

// fakeSimulator implements SimulatorEngine.
type fakeSimulator struct {
	whatIfFunc   func(ctx context.Context, req policy.WhatIfRequest) (*policy.WhatIfResult, error)
	dataFlowFunc func(ctx context.Context, req policy.WhatIfRequest) (*policy.DataFlowDiff, error)
}

func (f *fakeSimulator) WhatIf(ctx context.Context, req policy.WhatIfRequest) (*policy.WhatIfResult, error) {
	if f.whatIfFunc != nil {
		return f.whatIfFunc(ctx, req)
	}
	return &policy.WhatIfResult{}, nil
}

func (f *fakeSimulator) SimulateDataFlow(ctx context.Context, req policy.WhatIfRequest) (*policy.DataFlowDiff, error) {
	if f.dataFlowFunc != nil {
		return f.dataFlowFunc(ctx, req)
	}
	return &policy.DataFlowDiff{}, nil
}

func newSimulatorRouter(t *testing.T, store *fakeDraftStore, prom *fakePromotion, sim *fakeSimulator, ad admin.AuditWriter, tenantID string) *gin.Engine {
	t.Helper()
	h, err := admin.NewSimulatorHandler(admin.SimulatorConfig{
		Drafts: store, Promotion: prom, Simulator: sim, Audit: ad,
	})
	if err != nil {
		t.Fatalf("NewSimulatorHandler: %v", err)
	}
	r := gin.New()
	if tenantID != "" {
		r.Use(func(c *gin.Context) {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "actor-1")
			c.Next()
		})
	}
	rg := r.Group("/")
	h.Register(rg)
	return r
}

func TestSimulatorHandler_CreateDraft_Happy(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	ad := &fakeAudit{}
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, ad, "tenant-a")

	body := admin.CreateDraftRequest{
		ChannelID: "channel-1",
		Snapshot:  policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeRemote},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got policy.Draft
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("TenantID: %q", got.TenantID)
	}
	if got.Status != policy.DraftStatusDraft {
		t.Fatalf("Status: %q", got.Status)
	}
	actions := ad.actions()
	if len(actions) != 1 || actions[0] != audit.ActionPolicyDrafted {
		t.Fatalf("audit: %v", actions)
	}
}

func TestSimulatorHandler_CreateDraft_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "")

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_GetDraft_NotFound(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/policy/drafts/missing", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_GetDraft_TenantIsolation(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	d := policy.NewDraft("tenant-b", "", "ken", policy.PolicySnapshot{})
	if err := store.Create(context.Background(), d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/policy/drafts/"+d.ID, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant Get must be 404, got %d", w.Code)
	}
}

func TestSimulatorHandler_ListDrafts_TenantScoped(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	for _, tenant := range []string{"tenant-a", "tenant-a", "tenant-b"} {
		d := policy.NewDraft(tenant, "", "ken", policy.PolicySnapshot{})
		if err := store.Create(context.Background(), d); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/policy/drafts", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got struct {
		Drafts []policy.Draft `json:"drafts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Drafts) != 2 {
		t.Fatalf("expected 2 drafts for tenant-a, got %d", len(got.Drafts))
	}
}

func TestSimulatorHandler_PromoteDraft_Happy(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	prom := &fakePromotion{}
	r := newSimulatorRouter(t, store, prom, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts/d-1/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if prom.promoteCall != 1 {
		t.Fatalf("PromoteDraft calls: %d", prom.promoteCall)
	}
}

func TestSimulatorHandler_PromoteDraft_BlockedConflicts409(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	prom := &fakePromotion{
		promoteErr: &policy.ErrPromotionBlocked{Conflicts: []policy.PolicyConflict{{
			Severity: "error", Type: "acl_overlap",
			Description: "deny+allow on drive/**",
		}}},
	}
	r := newSimulatorRouter(t, store, prom, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts/d-1/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["conflicts"]; !ok {
		t.Fatalf("response should include conflicts: %s", w.Body.String())
	}
}

func TestSimulatorHandler_PromoteDraft_NotFound(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	prom := &fakePromotion{promoteErr: policy.ErrDraftNotFound}
	r := newSimulatorRouter(t, store, prom, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts/d-1/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_PromoteDraft_TerminalReturns409(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	prom := &fakePromotion{promoteErr: policy.ErrDraftTerminal}
	r := newSimulatorRouter(t, store, prom, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts/d-1/promote", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_RejectDraft_RecordsReason(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	prom := &fakePromotion{}
	r := newSimulatorRouter(t, store, prom, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.RejectDraftRequest{Reason: "out-of-date"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/drafts/d-1/reject", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if prom.rejectCall != 1 {
		t.Fatalf("RejectDraft calls: %d", prom.rejectCall)
	}
}

func TestSimulatorHandler_SimulateWhatIf_HydratesFromDraft(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{
		EffectiveMode: policy.PrivacyModeLocalOnly,
	})
	if err := store.Create(context.Background(), d); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var observed policy.WhatIfRequest
	sim := &fakeSimulator{
		whatIfFunc: func(_ context.Context, req policy.WhatIfRequest) (*policy.WhatIfResult, error) {
			observed = req
			return &policy.WhatIfResult{LiveResults: []policy.RetrieveHit{{ID: "x"}}}, nil
		},
	}
	r := newSimulatorRouter(t, store, &fakePromotion{}, sim, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{DraftID: d.ID, Query: "hello"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if observed.DraftPolicy.EffectiveMode != policy.PrivacyModeLocalOnly {
		t.Fatalf("snapshot not hydrated: %+v", observed.DraftPolicy)
	}
}

func TestSimulatorHandler_SimulateWhatIf_RejectsMissingQuery(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{DraftPolicy: policy.PolicySnapshot{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_SimulateWhatIf_DraftNotFound(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{DraftID: "missing", Query: "q"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestSimulatorHandler_SimulateWhatIf_DraftStoreInternalError(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	store.getErr = errors.New("connection reset by peer")
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{DraftID: "any", Query: "q"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestSimulatorHandler_SimulateDataFlow_DraftStoreInternalError(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	store.getErr = errors.New("db: deadline exceeded")
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{DraftID: "any", Query: "q"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate/diff", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestSimulatorHandler_SimulateDataFlow_Happy(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	sim := &fakeSimulator{
		dataFlowFunc: func(_ context.Context, _ policy.WhatIfRequest) (*policy.DataFlowDiff, error) {
			return &policy.DataFlowDiff{
				Live:  map[string]int{"remote": 4},
				Draft: map[string]int{"local-only": 4},
				Delta: map[string]int{"remote": -4, "local-only": 4},
			}, nil
		},
	}
	r := newSimulatorRouter(t, store, &fakePromotion{}, sim, &fakeAudit{}, "tenant-a")

	body := admin.SimulateRequest{Query: "q", DraftPolicy: policy.PolicySnapshot{EffectiveMode: policy.PrivacyModeLocalOnly}}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/simulate/diff", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestSimulatorHandler_DetectConflicts_Inline(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.ConflictsRequest{Snapshot: policy.PolicySnapshot{
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
			{PathGlob: "drive/**", Action: policy.ACLActionDeny},
		}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/conflicts", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Conflicts []policy.PolicyConflict `json:"conflicts"`
		HasError  bool                    `json:"has_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.HasError {
		t.Fatalf("expected has_error=true; conflicts=%+v", got.Conflicts)
	}
}

func TestSimulatorHandler_DetectConflicts_HydratesFromDraft(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	d := policy.NewDraft("tenant-a", "", "ken", policy.PolicySnapshot{
		ACL: &policy.AllowDenyList{Rules: []policy.ACLRule{
			{PathGlob: "drive/**", Action: policy.ACLActionAllow},
			{PathGlob: "drive/**", Action: policy.ACLActionDeny},
		}},
	})
	if err := store.Create(context.Background(), d); err != nil {
		t.Fatalf("Create: %v", err)
	}
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.ConflictsRequest{DraftID: d.ID}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/conflicts", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestSimulatorHandler_DetectConflicts_DraftNotFound(t *testing.T) {
	t.Parallel()
	store := newFakeDraftStore()
	r := newSimulatorRouter(t, store, &fakePromotion{}, &fakeSimulator{}, &fakeAudit{}, "tenant-a")

	body := admin.ConflictsRequest{DraftID: "missing"}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/policy/conflicts", mustJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestSimulatorHandler_NewSimulatorHandler_Validation(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewSimulatorHandler(admin.SimulatorConfig{}); err == nil {
		t.Fatal("expected error for missing Drafts")
	}
	if _, err := admin.NewSimulatorHandler(admin.SimulatorConfig{Drafts: newFakeDraftStore()}); err == nil {
		t.Fatal("expected error for missing Promotion")
	}
	if _, err := admin.NewSimulatorHandler(admin.SimulatorConfig{
		Drafts:    newFakeDraftStore(),
		Promotion: &fakePromotion{},
	}); err == nil {
		t.Fatal("expected error for missing Simulator")
	}
}

// silence unused imports in case the test file is shrunk.
var _ = errors.New
