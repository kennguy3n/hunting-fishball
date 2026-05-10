package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// rotatorRouter mounts a CredentialRotator and a fake auth
// middleware on a fresh engine, mirroring router() but without
// the source handler registration (the rotator is the handler
// under test).
func rotatorRouter(t *testing.T, r *admin.CredentialRotator, tenantID string) *gin.Engine {
	t.Helper()
	g := gin.New()
	if tenantID != "" {
		g.Use(func(c *gin.Context) {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "actor-1")
			c.Next()
		})
	}
	rg := g.Group("/")
	r.Register(rg)
	return g
}

func TestCredentialRotator_HappyPath(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{
		"credentials": []byte("OLD"),
		"site":        "x",
	}, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	rotator := &admin.CredentialRotator{
		Repo:      repo,
		Audit:     &fakeAudit{},
		Validator: &fakeValidator{},
		Now:       func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}
	res, err := rotator.Rotate(ctx, "tenant-a", src.ID, admin.CredentialRotateRequest{
		Credentials: []byte("NEW"),
		Reason:      "scheduled",
	})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if res.SourceID != src.ID {
		t.Fatalf("response source_id = %s; want %s", res.SourceID, src.ID)
	}
	want := time.Date(2026, 5, 10, 13, 0, 0, 0, time.UTC)
	if !res.PreviousExpiryAt.Equal(want) {
		t.Fatalf("previous_expiry = %v; want %v", res.PreviousExpiryAt, want)
	}
	got, err := repo.Get(ctx, "tenant-a", src.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Note: JSONMap unmarshal turns []byte into a base64 string so
	// the stored credentials may be string or []byte depending on
	// the dialect. We just need the swap to have taken effect.
	if got.Config["credentials"] == nil {
		t.Fatalf("credentials missing after rotate")
	}
	if got.Config["previous_credentials"] == nil {
		t.Fatalf("previous_credentials missing after rotate")
	}
	if got.Config["previous_credentials_expires_at"] == nil {
		t.Fatalf("previous_credentials_expires_at missing")
	}
}

func TestCredentialRotator_ValidatorError_NoMutation(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{"credentials": []byte("OLD")}, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	rotator := &admin.CredentialRotator{
		Repo:      repo,
		Audit:     &fakeAudit{},
		Validator: &fakeValidator{err: errors.New("bad creds")},
	}
	_, err := rotator.Rotate(ctx, "tenant-a", src.ID, admin.CredentialRotateRequest{Credentials: []byte("NEW")})
	if err == nil {
		t.Fatal("expected error")
	}
	got, _ := repo.Get(ctx, "tenant-a", src.ID)
	if got.Config["previous_credentials"] != nil {
		t.Fatalf("validation failure should NOT have mutated the row")
	}
}

func TestCredentialRotator_404OnUnknown(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	rotator := &admin.CredentialRotator{Repo: repo, Audit: &fakeAudit{}, Validator: &fakeValidator{}}
	_, err := rotator.Rotate(context.Background(), "tenant-a", "src-missing", admin.CredentialRotateRequest{Credentials: []byte("x")})
	if err == nil {
		t.Fatal("expected ErrSourceNotFound")
	}
}

func TestCredentialRotator_HTTP_Auth(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	rotator := &admin.CredentialRotator{Repo: repo, Audit: &fakeAudit{}, Validator: &fakeValidator{}}
	r := rotatorRouter(t, rotator, "" /* no tenant */)
	body := mustJSON(t, admin.CredentialRotateRequest{Credentials: []byte("x")})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/X/rotate-credentials", body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", w.Code)
	}
}

func TestCredentialRotator_HTTP_HappyPath(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{"credentials": []byte("OLD")}, nil)
	_ = repo.Create(context.Background(), src)
	ad := &fakeAudit{}
	rotator := &admin.CredentialRotator{Repo: repo, Audit: ad, Validator: &fakeValidator{}}
	r := rotatorRouter(t, rotator, "tenant-a")

	body := mustJSON(t, admin.CredentialRotateRequest{Credentials: []byte("NEW"), Reason: "incident"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/"+src.ID+"/rotate-credentials", body)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	acts := ad.actions()
	if len(acts) != 1 || acts[0] != audit.ActionSourceCredentialsRotated {
		t.Fatalf("audit actions = %v; want exactly source.credentials_rotated", acts)
	}
}
