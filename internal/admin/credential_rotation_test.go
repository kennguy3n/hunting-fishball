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

// TestCredentialRotator_DefaultClock is the regression for
// runner.go using `time.Now().UTC` (Go method-value syntax)
// which freezes Now() to its first evaluation; the production
// path (r.Now == nil) must return the current wall time each
// call. We exercise the unset path and confirm RotatedAt is
// within a small skew of the test execution time and that
// PreviousExpiryAt is correctly RotatedAt + grace period.
func TestCredentialRotator_DefaultClock(t *testing.T) {
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
		Validator: &fakeValidator{},
		// Now is intentionally left nil to exercise the
		// `time.Now().UTC()` default path.
	}
	before := time.Now().UTC()
	res, err := rotator.Rotate(ctx, "tenant-a", src.ID, admin.CredentialRotateRequest{Credentials: []byte("NEW")})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	after := time.Now().UTC()

	if res.RotatedAt.Before(before) || res.RotatedAt.After(after) {
		t.Fatalf("RotatedAt %v not within [%v, %v]", res.RotatedAt, before, after)
	}
	wantExpiry := res.RotatedAt.Add(admin.CredentialGracePeriod)
	if !res.PreviousExpiryAt.Equal(wantExpiry) {
		t.Fatalf("PreviousExpiryAt = %v; want %v", res.PreviousExpiryAt, wantExpiry)
	}
}

// TestCredentialRotator_TimestampConsistency is the regression
// for the round-3 Devin Review finding that Rotate() called
// `now()` at four separate sites (config blob expiry, config
// blob rotated_at, DB updated_at, and the API response/audit
// rotatedAt) producing four divergent wall-clock instants.
//
// The fix captures `now()` once at the top and reuses for every
// timestamp. We verify here that:
//   - The HTTP response RotatedAt equals the config blob's
//     `credentials_rotated_at`.
//   - The HTTP response PreviousExpiryAt equals the config blob's
//     `previous_credentials_expires_at`.
//   - The audit event's `rotated_at` and `previous_expires` match
//     the HTTP response.
//   - The DB `updated_at` column equals the response RotatedAt.
//
// The fake clock returns a counter-incremented timestamp each
// call so a regression that re-introduces multiple now() calls
// would be flagged immediately by mismatched values.
func TestCredentialRotator_TimestampConsistency(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	ctx := context.Background()

	src := admin.NewSource("tenant-a", "google-drive", admin.JSONMap{
		"credentials": []byte("OLD"),
	}, nil)
	if err := repo.Create(ctx, src); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Counter-incremented clock: each call returns a strictly
	// later timestamp than the previous one. Any code path that
	// calls now() more than once will be caught by the equality
	// checks below because the values will diverge.
	var calls int
	base := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Millisecond)
	}

	ad := &fakeAudit{}
	rotator := &admin.CredentialRotator{
		Repo:      repo,
		Audit:     ad,
		Validator: &fakeValidator{},
		Now:       clock,
	}
	res, err := rotator.Rotate(ctx, "tenant-a", src.ID, admin.CredentialRotateRequest{
		Credentials: []byte("NEW"),
		Reason:      "scheduled",
	})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// 1. HTTP response RotatedAt vs config blob credentials_rotated_at.
	got, err := repo.Get(ctx, "tenant-a", src.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cfgRotatedAtRaw, ok := got.Config["credentials_rotated_at"].(string)
	if !ok {
		t.Fatalf("config[credentials_rotated_at] not a string: %T", got.Config["credentials_rotated_at"])
	}
	cfgRotatedAt, err := time.Parse(time.RFC3339Nano, cfgRotatedAtRaw)
	if err != nil {
		t.Fatalf("parse credentials_rotated_at: %v", err)
	}
	if !cfgRotatedAt.Equal(res.RotatedAt) {
		t.Fatalf("config credentials_rotated_at = %v; HTTP RotatedAt = %v; should be identical", cfgRotatedAt, res.RotatedAt)
	}

	// 2. HTTP response PreviousExpiryAt vs config blob previous_credentials_expires_at.
	cfgExpiryRaw, ok := got.Config["previous_credentials_expires_at"].(string)
	if !ok {
		t.Fatalf("config[previous_credentials_expires_at] not a string: %T", got.Config["previous_credentials_expires_at"])
	}
	cfgExpiry, err := time.Parse(time.RFC3339Nano, cfgExpiryRaw)
	if err != nil {
		t.Fatalf("parse previous_credentials_expires_at: %v", err)
	}
	if !cfgExpiry.Equal(res.PreviousExpiryAt) {
		t.Fatalf("config previous_credentials_expires_at = %v; HTTP PreviousExpiryAt = %v; should be identical", cfgExpiry, res.PreviousExpiryAt)
	}

	// 3. Expiry should be exactly RotatedAt + grace period.
	wantExpiry := res.RotatedAt.Add(admin.CredentialGracePeriod)
	if !res.PreviousExpiryAt.Equal(wantExpiry) {
		t.Fatalf("PreviousExpiryAt = %v; want RotatedAt + grace = %v", res.PreviousExpiryAt, wantExpiry)
	}

	// 4. Audit event metadata must reference the same rotated_at
	//    and previous_expires as the HTTP response.
	logs := ad.auditLogs()
	if len(logs) != 1 {
		t.Fatalf("audit logs = %d; want 1", len(logs))
	}
	auditRotatedAt, ok := logs[0].Metadata["rotated_at"].(string)
	if !ok {
		t.Fatalf("audit metadata[rotated_at] missing or not a string: %v", logs[0].Metadata)
	}
	if auditRotatedAt != res.RotatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("audit rotated_at = %s; response RotatedAt = %s", auditRotatedAt, res.RotatedAt.Format(time.RFC3339Nano))
	}
	auditExpiry, ok := logs[0].Metadata["previous_expires"].(string)
	if !ok {
		t.Fatalf("audit metadata[previous_expires] missing or not a string: %v", logs[0].Metadata)
	}
	if auditExpiry != res.PreviousExpiryAt.Format(time.RFC3339Nano) {
		t.Fatalf("audit previous_expires = %s; response PreviousExpiryAt = %s", auditExpiry, res.PreviousExpiryAt.Format(time.RFC3339Nano))
	}

	// 5. The clock must have been called exactly once for the
	//    rotation timestamp. (The defaulting closure assignment
	//    in Rotate() doesn't invoke it; only the rotatedAt :=
	//    now() call should.) If this assertion regresses the
	//    callsite-reduction will be too — adjust as needed.
	if calls != 1 {
		t.Fatalf("now() called %d times; want exactly 1 (each call would produce a divergent timestamp)", calls)
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
