package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// fakeRefresher captures the input params and returns a canned
// RefreshResult / error.
type fakeRefresher struct {
	calls   int
	last    admin.RefreshParams
	result  admin.RefreshResult
	err     error
	perCall map[string]admin.RefreshResult
	errFor  map[string]error
}

func (f *fakeRefresher) Refresh(_ context.Context, p admin.RefreshParams) (admin.RefreshResult, error) {
	f.calls++
	f.last = p
	if f.errFor != nil {
		if e, ok := f.errFor[p.RefreshToken]; ok {
			return admin.RefreshResult{}, e
		}
	}
	if f.perCall != nil {
		if r, ok := f.perCall[p.RefreshToken]; ok {
			return r, nil
		}
	}
	if f.err != nil {
		return admin.RefreshResult{}, f.err
	}
	return f.result, nil
}

// fakeListUpdater satisfies both SourceLister and SourceConfigUpdater
// without spinning up the SQLite-backed repo (we want every test to
// be free of GORM JSONMap quirks for credentials sub-maps).
type fakeListUpdater struct {
	sources []admin.Source
	updates map[string]admin.JSONMap
	err     error
}

func newFakeListUpdater(sources []admin.Source) *fakeListUpdater {
	return &fakeListUpdater{
		sources: sources,
		updates: make(map[string]admin.JSONMap),
	}
}

func (f *fakeListUpdater) ListAllActive(_ context.Context) ([]admin.Source, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sources, nil
}

func (f *fakeListUpdater) UpdateConfig(_ context.Context, _, id string, cfg admin.JSONMap) error {
	f.updates[id] = cfg
	for i := range f.sources {
		if f.sources[i].ID == id {
			f.sources[i].Config = cfg
		}
	}
	return nil
}

func newOAuthSource(id, tenantID, connector string, expiresAt time.Time, refreshToken string) admin.Source {
	creds := map[string]any{
		"access_token":  "old-access",
		"refresh_token": refreshToken,
		"token_url":     "https://oauth.example/token",
		"client_id":     "cid",
		"client_secret": "csecret",
	}
	if !expiresAt.IsZero() {
		creds["expires_at"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	return admin.Source{
		ID:            id,
		TenantID:      tenantID,
		ConnectorType: connector,
		Status:        admin.SourceStatusActive,
		Config: admin.JSONMap{
			"credentials": creds,
		},
	}
}

func TestTokenRefreshWorker_RefreshesWithinWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	src := newOAuthSource("01HQYK0000000000000000XYZ", "tenant-a", "google-drive",
		now.Add(2*time.Minute), "refresh-token-a")
	store := newFakeListUpdater([]admin.Source{src})
	ad := &fakeAudit{}
	ref := &fakeRefresher{result: admin.RefreshResult{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		ExpiresAt:    now.Add(time.Hour),
	}}
	w, err := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister:    store,
		Updater:   store,
		Refresher: ref,
		Audit:     ad,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTokenRefreshWorker: %v", err)
	}
	stats, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stats.Refreshed != 1 || stats.Skipped != 0 || stats.Errors != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if ref.last.RefreshToken != "refresh-token-a" {
		t.Fatalf("refresh token forwarded: %q", ref.last.RefreshToken)
	}
	updated, ok := store.updates[src.ID]
	if !ok {
		t.Fatalf("UpdateConfig not called")
	}
	creds := updated["credentials"].(map[string]any)
	if creds["access_token"] != "new-access" {
		t.Fatalf("access_token not updated: %+v", creds)
	}
	if creds["refresh_token"] != "new-refresh" {
		t.Fatalf("refresh_token not rotated: %+v", creds)
	}
	if creds["expires_at"] != now.Add(time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("expires_at: %v", creds["expires_at"])
	}
	if updated["credentials_rotated_at"] != now.Format(time.RFC3339Nano) {
		t.Fatalf("credentials_rotated_at: %v", updated["credentials_rotated_at"])
	}
	if got := ad.actions(); len(got) != 1 || got[0] != "source.token_refreshed" {
		t.Fatalf("audit actions: %v", got)
	}
}

func TestTokenRefreshWorker_SkipsOutsideWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	// Expires in 30 minutes — well outside the 5-minute window.
	src := newOAuthSource("S2", "tenant-a", "google-drive", now.Add(30*time.Minute), "refresh-b")
	store := newFakeListUpdater([]admin.Source{src})
	ref := &fakeRefresher{}
	w, _ := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister:    store,
		Updater:   store,
		Refresher: ref,
		Now:       func() time.Time { return now },
	})
	stats, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stats.Refreshed != 0 || stats.Skipped != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if ref.calls != 0 {
		t.Fatalf("Refresher must not be called when token is fresh")
	}
}

func TestTokenRefreshWorker_SkipsNonOAuthSources(t *testing.T) {
	t.Parallel()
	src := admin.Source{
		ID:            "S3",
		TenantID:      "tenant-a",
		ConnectorType: "static-credentials",
		Status:        admin.SourceStatusActive,
		Config:        admin.JSONMap{"credentials": map[string]any{"access_token": "static-only"}},
	}
	store := newFakeListUpdater([]admin.Source{src})
	ref := &fakeRefresher{}
	w, _ := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister: store, Updater: store, Refresher: ref,
	})
	stats, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stats.Refreshed != 0 || stats.Skipped != 1 || ref.calls != 0 {
		t.Fatalf("stats: %+v calls=%d", stats, ref.calls)
	}
}

func TestTokenRefreshWorker_ValidationErrorClassification(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	src := newOAuthSource("S4", "tenant-a", "google-drive", now.Add(time.Minute), "refresh-c")
	store := newFakeListUpdater([]admin.Source{src})
	ref := &fakeRefresher{err: &admin.RefreshValidationError{Err: errors.New("invalid_grant")}}
	w, _ := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister: store, Updater: store, Refresher: ref,
		Now: func() time.Time { return now },
	})
	stats, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stats.Errors != 1 || stats.Refreshed != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if _, ok := store.updates[src.ID]; ok {
		t.Fatal("UpdateConfig must not run after refresh validation error")
	}
}

func TestTokenRefreshWorker_TransportError(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	src := newOAuthSource("S5", "tenant-a", "google-drive", now.Add(time.Minute), "refresh-d")
	store := newFakeListUpdater([]admin.Source{src})
	ref := &fakeRefresher{err: errors.New("dial: connection refused")}
	w, _ := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister: store, Updater: store, Refresher: ref,
		Now: func() time.Time { return now },
	})
	stats, err := w.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if stats.Errors != 1 {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestTokenRefreshWorker_PreservesUnchangedRefreshToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	src := newOAuthSource("S6", "tenant-a", "google-drive", now.Add(time.Minute), "stable-refresh")
	store := newFakeListUpdater([]admin.Source{src})
	// Provider responded with no refresh_token rotation — worker
	// must NOT clobber the existing refresh_token.
	ref := &fakeRefresher{result: admin.RefreshResult{
		AccessToken: "fresh-access",
		ExpiresAt:   now.Add(time.Hour),
	}}
	w, _ := admin.NewTokenRefreshWorker(admin.TokenRefreshWorkerConfig{
		Lister: store, Updater: store, Refresher: ref,
		Now: func() time.Time { return now },
	})
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	updated := store.updates[src.ID]
	creds := updated["credentials"].(map[string]any)
	if creds["refresh_token"] != "stable-refresh" {
		t.Fatalf("refresh_token clobbered: %v", creds["refresh_token"])
	}
}

// TestOAuth2RefreshClient_HappyPath drives the production
// httptest-backed client through a successful refresh-token grant.
func TestOAuth2RefreshClient_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "grant_type=refresh_token") {
			t.Errorf("missing grant_type: %s", body)
		}
		if !strings.Contains(string(body), "refresh_token=rt") {
			t.Errorf("missing refresh_token: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-tok",
			"refresh_token": "new-rt",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	client := admin.NewOAuth2RefreshClient(srv.Client())
	res, err := client.Refresh(context.Background(), admin.RefreshParams{
		TokenURL:     srv.URL,
		RefreshToken: "rt",
		ClientID:     "cid",
		ClientSecret: "secret",
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.AccessToken != "new-tok" || res.RefreshToken != "new-rt" {
		t.Fatalf("response: %+v", res)
	}
	if res.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt should be derived from expires_in")
	}
}

func TestOAuth2RefreshClient_ProviderRejection(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}))
	t.Cleanup(srv.Close)
	client := admin.NewOAuth2RefreshClient(srv.Client())
	_, err := client.Refresh(context.Background(), admin.RefreshParams{
		TokenURL:     srv.URL,
		RefreshToken: "rt",
	})
	var ve *admin.RefreshValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected RefreshValidationError, got %T: %v", err, err)
	}
}

func TestOAuth2RefreshClient_RejectsEmptyToken(t *testing.T) {
	t.Parallel()
	client := admin.NewOAuth2RefreshClient(nil)
	_, err := client.Refresh(context.Background(), admin.RefreshParams{TokenURL: "https://x"})
	var ve *admin.RefreshValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected RefreshValidationError, got %v", err)
	}
}
