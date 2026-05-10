package shard_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// fakeRepo is a fixture HandlerRepo implementation that lets each
// test seed in-memory state without spinning up a real database.
type fakeRepo struct {
	manifests map[string]*shard.ShardManifest // keyed by ID
	chunks    map[string][]string             // shard_id → chunk_id
	listErr   error
	listFn    func(ctx context.Context, f shard.ScopeFilter, pageSize int) ([]shard.ShardManifest, error)
}

func (r *fakeRepo) List(ctx context.Context, f shard.ScopeFilter, pageSize int) ([]shard.ShardManifest, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	if r.listFn != nil {
		return r.listFn(ctx, f, pageSize)
	}
	out := []shard.ShardManifest{}
	for _, m := range r.manifests {
		if m.TenantID != f.TenantID {
			continue
		}
		if f.UserID != "" && m.UserID != f.UserID {
			continue
		}
		if f.ChannelID != "" && m.ChannelID != f.ChannelID {
			continue
		}
		if f.Status != "" && m.Status != f.Status {
			continue
		}
		if f.PrivacyMode != "" && m.PrivacyMode != f.PrivacyMode {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

func (r *fakeRepo) GetByVersion(_ context.Context, f shard.ScopeFilter, version int64) (*shard.ShardManifest, error) {
	for _, m := range r.manifests {
		if m.TenantID == f.TenantID &&
			m.PrivacyMode == f.PrivacyMode &&
			m.UserID == f.UserID &&
			m.ChannelID == f.ChannelID &&
			m.ShardVersion == version {
			cp := *m
			return &cp, nil
		}
	}
	return nil, shard.ErrShardNotFound
}

func (r *fakeRepo) ChunkIDs(_ context.Context, _ string, shardID string) ([]string, error) {
	return r.chunks[shardID], nil
}

func (r *fakeRepo) LatestVersion(_ context.Context, f shard.ScopeFilter) (int64, error) {
	var max int64
	for _, m := range r.manifests {
		if m.TenantID == f.TenantID &&
			m.PrivacyMode == f.PrivacyMode &&
			m.UserID == f.UserID &&
			m.ChannelID == f.ChannelID &&
			m.ShardVersion > max {
			max = m.ShardVersion
		}
	}
	return max, nil
}

type fakeForget struct {
	called   bool
	tenantID string
	actor    string
	err      error
}

func (f *fakeForget) Forget(_ context.Context, tenantID, requestedBy string) error {
	f.called = true
	f.tenantID = tenantID
	f.actor = requestedBy
	return f.err
}

func newTestHandler(t *testing.T, repo *fakeRepo, forget shard.ForgetTrigger) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Inject the authenticated tenant + actor as the auth middleware would.
	r.Use(func(c *gin.Context) {
		if v := c.Request.Header.Get("X-Tenant-ID"); v != "" {
			c.Set(audit.TenantContextKey, v)
		}
		if v := c.Request.Header.Get("X-Actor-ID"); v != "" {
			c.Set(audit.ActorContextKey, v)
		}
		c.Next()
	})
	h, err := shard.NewHandler(shard.HandlerConfig{Repo: repo, Forget: forget})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	rg := r.Group("")
	h.Register(rg)
	return r
}

func TestHandler_ListShards(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		manifests: map[string]*shard.ShardManifest{
			"01": {ID: "01", TenantID: "tenant-a", UserID: "user-1", PrivacyMode: "internal", ShardVersion: 1, Status: shard.ShardStatusReady},
			"02": {ID: "02", TenantID: "tenant-a", UserID: "user-1", PrivacyMode: "internal", ShardVersion: 2, Status: shard.ShardStatusReady},
			"03": {ID: "03", TenantID: "tenant-b", UserID: "user-1", PrivacyMode: "internal", ShardVersion: 1, Status: shard.ShardStatusReady},
		},
	}
	r := newTestHandler(t, repo, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Shards []shard.ShardManifest `json:"shards"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Shards) != 2 {
		t.Fatalf("want 2 (only tenant-a's), got %d", len(resp.Shards))
	}
}

func TestHandler_ListShards_TenantMismatch(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandler_ListShards_MissingContext(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandler_Delta(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		manifests: map[string]*shard.ShardManifest{
			"v1": {ID: "v1", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 1, Status: shard.ShardStatusReady},
			"v2": {ID: "v2", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 2, Status: shard.ShardStatusReady},
		},
		chunks: map[string][]string{
			"v1": {"a", "b", "c"},
			"v2": {"b", "c", "d"},
		},
	}
	r := newTestHandler(t, repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/delta?since=1&privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		From       int64           `json:"from_version"`
		To         int64           `json:"to_version"`
		Operations []shard.DeltaOp `json:"operations"`
		IsFullSync bool            `json:"is_full_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.From != 1 || resp.To != 2 {
		t.Fatalf("from/to: %d/%d", resp.From, resp.To)
	}
	if resp.IsFullSync {
		t.Fatal("expected partial sync")
	}
	if len(resp.Operations) != 2 {
		t.Fatalf("want 2 ops, got %d: %v", len(resp.Operations), resp.Operations)
	}
}

func TestHandler_Delta_FullSync(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		manifests: map[string]*shard.ShardManifest{
			"v2": {ID: "v2", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 2, Status: shard.ShardStatusReady},
		},
		chunks: map[string][]string{"v2": {"a", "b"}},
	}
	r := newTestHandler(t, repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/delta?since=0&privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Operations []shard.DeltaOp `json:"operations"`
		IsFullSync bool            `json:"is_full_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.IsFullSync {
		t.Fatalf("expected full sync, got %+v", resp)
	}
	if len(resp.Operations) != 2 {
		t.Fatalf("want 2 adds, got %d", len(resp.Operations))
	}
}

// TestHandler_Delta_AlreadyAtLatest exercises the case where a
// client's `since` is already at (or beyond) the freshest manifest.
// We must NOT re-emit every chunk as an add — the device is already
// up to date. The response should be empty operations and is_full_sync
// false.
func TestHandler_Delta_AlreadyAtLatest(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		manifests: map[string]*shard.ShardManifest{
			"v3": {ID: "v3", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 3, Status: shard.ShardStatusReady},
		},
		chunks: map[string][]string{"v3": {"a", "b", "c", "d", "e"}},
	}
	r := newTestHandler(t, repo, nil)

	for _, since := range []string{"3", "4"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/delta?since="+since+"&privacy_mode=internal", nil)
		req.Header.Set("X-Tenant-ID", "tenant-a")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("since=%s status=%d body=%s", since, rec.Code, rec.Body.String())
		}
		var resp struct {
			From       int64           `json:"from_version"`
			To         int64           `json:"to_version"`
			Operations []shard.DeltaOp `json:"operations"`
			IsFullSync bool            `json:"is_full_sync"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.IsFullSync {
			t.Fatalf("since=%s: expected partial (empty) sync, got full sync", since)
		}
		if len(resp.Operations) != 0 {
			t.Fatalf("since=%s: expected no operations when already at latest, got %d: %+v", since, len(resp.Operations), resp.Operations)
		}
		if resp.To != 3 {
			t.Fatalf("since=%s: expected to=3, got %d", since, resp.To)
		}
	}
}

func TestHandler_Delta_RequiresPrivacyMode(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/delta?since=0", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_Forget(t *testing.T) {
	t.Parallel()
	forget := &fakeForget{}
	r := newTestHandler(t, &fakeRepo{}, forget)

	req := httptest.NewRequest(http.MethodDelete, "/v1/tenants/tenant-a/keys", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	req.Header.Set("X-Actor-ID", "admin-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !forget.called || forget.tenantID != "tenant-a" || forget.actor != "admin-1" {
		t.Fatalf("forget not invoked correctly: %+v", forget)
	}
	// On a successful nil-error return from Forget, the orchestrator
	// has already swept every storage tier and stamped the row as
	// `deleted`. The handler must echo that terminal state — surfacing
	// `pending_deletion` here would mislead clients into thinking the
	// workflow was still running.
	var body struct {
		TenantID string `json:"tenant_id"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != string(shard.LifecycleDeleted) {
		t.Fatalf("expected state=%q, got %q", shard.LifecycleDeleted, body.State)
	}
	if body.TenantID != "tenant-a" {
		t.Fatalf("expected tenant_id=tenant-a, got %q", body.TenantID)
	}
}

func TestHandler_Forget_PropagatesError(t *testing.T) {
	t.Parallel()
	forget := &fakeForget{err: errors.New("boom")}
	r := newTestHandler(t, &fakeRepo{}, forget)

	req := httptest.NewRequest(http.MethodDelete, "/v1/tenants/tenant-a/keys", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

// fakeCoverageRepo extends fakeRepo with a CoverageRepo so the
// coverage endpoint test can drive the corpus-size denominator.
type fakeCoverageRepo struct {
	fakeRepo
	corpus    int
	corpusErr error
}

func (r *fakeCoverageRepo) CorpusChunkCount(_ context.Context, _ shard.ScopeFilter) (int, error) {
	if r.corpusErr != nil {
		return 0, r.corpusErr
	}
	return r.corpus, nil
}

func TestHandler_Coverage_NoShard(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/coverage?privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body shard.CoverageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Reason != "no_shard" {
		t.Fatalf("reason=%q", body.Reason)
	}
	if body.IsAuthoritative {
		t.Fatalf("expected non-authoritative")
	}
}

func TestHandler_Coverage_RequiresPrivacyMode(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/coverage", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_Coverage_TenantMismatch(t *testing.T) {
	t.Parallel()
	r := newTestHandler(t, &fakeRepo{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/coverage?privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestHandler_Coverage_NoCoverageRepo(t *testing.T) {
	t.Parallel()
	repo := &fakeRepo{
		manifests: map[string]*shard.ShardManifest{
			"01": {ID: "01", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 7, ChunksCount: 80, Status: shard.ShardStatusReady},
		},
	}
	r := newTestHandler(t, repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/coverage?privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body shard.CoverageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ShardChunks != 80 {
		t.Fatalf("shard_chunks=%d", body.ShardChunks)
	}
	if body.IsAuthoritative {
		t.Fatalf("expected non-authoritative when CoverageRepo is missing")
	}
	if body.Reason != "corpus_size_unknown" {
		t.Fatalf("reason=%q", body.Reason)
	}
}

func TestHandler_Coverage_AuthoritativeRatio(t *testing.T) {
	t.Parallel()
	repo := &fakeCoverageRepo{
		fakeRepo: fakeRepo{
			manifests: map[string]*shard.ShardManifest{
				"01": {ID: "01", TenantID: "tenant-a", PrivacyMode: "internal", ShardVersion: 4, ChunksCount: 60, Status: shard.ShardStatusReady},
			},
		},
		corpus: 200,
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		if v := c.Request.Header.Get("X-Tenant-ID"); v != "" {
			c.Set(audit.TenantContextKey, v)
		}
		c.Next()
	})
	h, herr := shard.NewHandler(shard.HandlerConfig{Repo: repo})
	if herr != nil {
		t.Fatalf("NewHandler: %v", herr)
	}
	rg := engine.Group("")
	h.Register(rg)

	req := httptest.NewRequest(http.MethodGet, "/v1/shards/tenant-a/coverage?privacy_mode=internal", nil)
	req.Header.Set("X-Tenant-ID", "tenant-a")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body shard.CoverageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ShardChunks != 60 || body.CorpusChunks != 200 {
		t.Fatalf("counts: shard=%d corpus=%d", body.ShardChunks, body.CorpusChunks)
	}
	want := 60.0 / 200.0
	if body.CoverageRatio < want-1e-9 || body.CoverageRatio > want+1e-9 {
		t.Fatalf("ratio: got %v want %v", body.CoverageRatio, want)
	}
	if !body.IsAuthoritative {
		t.Fatalf("expected authoritative")
	}
}

// Compile-time checks that the fixture satisfies the production
// interface — keeps the test honest if HandlerRepo grows.
var _ shard.HandlerRepo = (*fakeRepo)(nil)
var _ shard.HandlerRepo = (*fakeCoverageRepo)(nil)
var _ shard.CoverageRepo = (*fakeCoverageRepo)(nil)

func TestHandler_NewHandler_RequiresRepo(t *testing.T) {
	t.Parallel()
	if _, err := shard.NewHandler(shard.HandlerConfig{}); err == nil {
		t.Fatal("expected error for nil repo")
	}
}

// silence unused-import / vet warnings in scenarios we don't exercise
// directly here.
var _ = fmt.Sprintf
