package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

const sqliteDraftSchemaForHistory = `
CREATE TABLE policy_drafts (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    channel_id    TEXT,
    payload       TEXT NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL DEFAULT 'draft',
    created_by    TEXT,
    created_at    DATETIME NOT NULL,
    promoted_at   DATETIME,
    promoted_by   TEXT,
    reject_reason TEXT
);

CREATE TABLE policy_versions (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    channel_id  TEXT NOT NULL DEFAULT '',
    draft_id    TEXT NOT NULL,
    action      TEXT NOT NULL,
    actor_id    TEXT NOT NULL DEFAULT '',
    snapshot    TEXT NOT NULL DEFAULT '{}',
    created_at  DATETIME NOT NULL
);
`

type historyAuditFake struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (f *historyAuditFake) Create(_ context.Context, log *audit.AuditLog) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if log == nil {
		return errors.New("nil log")
	}
	f.logs = append(f.logs, log)
	return nil
}

func (f *historyAuditFake) CreateInTx(_ context.Context, tx *gorm.DB, log *audit.AuditLog) error {
	if tx == nil {
		return errors.New("nil tx")
	}
	return f.Create(context.Background(), log)
}

func (f *historyAuditFake) actions() []audit.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Action, len(f.logs))
	for i, l := range f.logs {
		out[i] = l.Action
	}
	return out
}

type historyLiveStoreFake struct{}

func (historyLiveStoreFake) ApplySnapshot(_ context.Context, _ *gorm.DB, _, _ string, _ policy.PolicySnapshot) error {
	return nil
}

func newHistoryRig(t *testing.T, tenantID string) (*gin.Engine, *policy.PolicyVersionRepository, *policy.DraftRepository, *historyAuditFake) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteDraftSchemaForHistory).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	versions := policy.NewPolicyVersionRepository(db)
	drafts := policy.NewDraftRepository(db)
	au := &historyAuditFake{}
	prom, err := policy.NewPromoter(policy.PromotionConfig{
		Drafts: drafts, LiveStore: historyLiveStoreFake{}, Audit: au, Versions: versions,
	})
	if err != nil {
		t.Fatalf("NewPromoter: %v", err)
	}
	h, err := admin.NewPolicyHistoryHandler(admin.PolicyHistoryConfig{
		Versions: versions, Drafts: drafts, Promoter: prom, Audit: au,
	})
	if err != nil {
		t.Fatalf("NewPolicyHistoryHandler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "admin-1")
		}
		c.Next()
	})
	rg := r.Group("/v1/admin")
	h.Register(rg)
	return r, versions, drafts, au
}

func seedPromotedDraft(t *testing.T, drafts *policy.DraftRepository, versions *policy.PolicyVersionRepository, tenantID string, mode policy.PrivacyMode) string {
	t.Helper()
	d := policy.NewDraft(tenantID, "channel-1", "ken", policy.PolicySnapshot{EffectiveMode: mode})
	if err := drafts.Create(context.Background(), d); err != nil {
		t.Fatalf("Create draft: %v", err)
	}
	v := &policy.PolicyVersion{
		TenantID: tenantID, ChannelID: "channel-1", DraftID: d.ID,
		Action: string(audit.ActionPolicyPromoted), ActorID: "ken",
		Snapshot: policy.DraftPayload{Snapshot: policy.PolicySnapshot{EffectiveMode: mode}},
	}
	if err := versions.Insert(context.Background(), nil, v); err != nil {
		t.Fatalf("Insert version: %v", err)
	}
	return v.ID
}

func TestPolicyHistory_List_HappyPath(t *testing.T) {
	t.Parallel()
	r, versions, drafts, _ := newHistoryRig(t, "tenant-a")
	for i := 0; i < 3; i++ {
		seedPromotedDraft(t, drafts, versions, "tenant-a", policy.PrivacyModeRemote)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/policy/history?limit=10", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp admin.PolicyHistoryListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(resp.Items))
	}
}

func TestPolicyHistory_List_TenantIsolation(t *testing.T) {
	t.Parallel()
	r, versions, drafts, _ := newHistoryRig(t, "tenant-a")
	seedPromotedDraft(t, drafts, versions, "tenant-a", policy.PrivacyModeRemote)
	seedPromotedDraft(t, drafts, versions, "tenant-b", policy.PrivacyModeRemote)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/policy/history", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp admin.PolicyHistoryListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Items) != 1 || resp.Items[0].TenantID != "tenant-a" {
		t.Fatalf("tenant isolation broken: %+v", resp.Items)
	}
}

func TestPolicyHistory_List_MissingTenantReturns401(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newHistoryRig(t, "")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/policy/history", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestPolicyHistory_Rollback_PromotesNewDraftFromSnapshot(t *testing.T) {
	t.Parallel()
	r, versions, drafts, au := newHistoryRig(t, "tenant-a")
	versionID := seedPromotedDraft(t, drafts, versions, "tenant-a", policy.PrivacyModeLocalOnly)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/policy/rollback/"+versionID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("rollback failed: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.PolicyRollbackResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NewDraftID == "" {
		t.Fatalf("expected new draft id")
	}
	if resp.Draft == nil || resp.Draft.Status != policy.DraftStatusPromoted {
		t.Fatalf("expected promoted draft, got %+v", resp.Draft)
	}
	if resp.Draft.Payload.Snapshot.EffectiveMode != policy.PrivacyModeLocalOnly {
		t.Fatalf("snapshot mode: %q", resp.Draft.Payload.Snapshot.EffectiveMode)
	}

	// Audit feed must record both the promote (from Promoter) and
	// the rollback intent (from the handler).
	got := au.actions()
	hasPromoted, hasRolledBack := false, false
	for _, a := range got {
		if a == audit.ActionPolicyPromoted {
			hasPromoted = true
		}
		if a == audit.ActionPolicyRolledBack {
			hasRolledBack = true
		}
	}
	if !hasPromoted || !hasRolledBack {
		t.Fatalf("expected promote+rolled_back actions, got %v", got)
	}

	// A new policy_versions row was appended for the promote.
	rows, err := versions.List(context.Background(), policy.VersionListFilter{TenantID: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 version rows after rollback, got %d", len(rows))
	}
}

func TestPolicyHistory_Rollback_UnknownVersionReturns404(t *testing.T) {
	t.Parallel()
	r, _, _, _ := newHistoryRig(t, "tenant-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/policy/rollback/01H000000000000000000NOTFND", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestPolicyHistory_Rollback_CrossTenantReturns404(t *testing.T) {
	t.Parallel()
	// First rig (tenant-a) seeds a version then we hit it with tenant-b.
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteDraftSchemaForHistory).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	versions := policy.NewPolicyVersionRepository(db)
	drafts := policy.NewDraftRepository(db)
	au := &historyAuditFake{}
	prom, _ := policy.NewPromoter(policy.PromotionConfig{
		Drafts: drafts, LiveStore: historyLiveStoreFake{}, Audit: au, Versions: versions,
	})
	h, _ := admin.NewPolicyHistoryHandler(admin.PolicyHistoryConfig{
		Versions: versions, Drafts: drafts, Promoter: prom, Audit: au,
	})
	versionID := seedPromotedDraft(t, drafts, versions, "tenant-a", policy.PrivacyModeRemote)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-b")
		c.Set(audit.ActorContextKey, "admin-b")
		c.Next()
	})
	rg := r.Group("/v1/admin")
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/policy/rollback/"+versionID, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant rollback should 404, got %d", w.Code)
	}
}
