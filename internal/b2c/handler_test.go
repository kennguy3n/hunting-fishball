package b2c_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/b2c"
)

func init() { gin.SetMode(gin.TestMode) }

// newRouter wires the handler with an auth shim that injects the
// supplied tenant id, so tests don't drag the production auth
// middleware along.
func newRouter(t *testing.T, h *b2c.Handler, tenantID string) *gin.Engine {
	t.Helper()
	r := gin.New()
	api := r.Group("/", func(c *gin.Context) {
		if tenantID != "" {
			c.Set(audit.TenantContextKey, tenantID)
		}
		c.Next()
	})
	h.Register(api)
	return r
}

func TestHandler_Health_ReturnsStatusOK(t *testing.T) {
	t.Parallel()
	h, err := b2c.NewHandler(b2c.HandlerConfig{ServerVersion: "v0.1.0"})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got b2c.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("status field=%q", got.Status)
	}
	if got.Version != "v0.1.0" {
		t.Fatalf("version=%q", got.Version)
	}
	if got.Time.IsZero() {
		t.Fatalf("time was zero")
	}
}

func TestHandler_Capabilities_ListsBackendsAndModes(t *testing.T) {
	t.Parallel()
	h, _ := b2c.NewHandler(b2c.HandlerConfig{
		EnabledBackends: []string{"vector", "bm25"},
		LocalShardSync:  true,
		DeviceFirst:     true,
		ServerVersion:   "v0.1.0",
	})
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got b2c.Capabilities
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.EnabledBackends) != 2 || got.EnabledBackends[0] != "vector" || got.EnabledBackends[1] != "bm25" {
		t.Fatalf("backends=%v", got.EnabledBackends)
	}
	if !got.LocalShardSync {
		t.Fatalf("expected local shard sync=true")
	}
	if !got.DeviceFirst {
		t.Fatalf("expected device-first=true")
	}
	if !got.PrivacyStrip {
		t.Fatalf("privacy strip should always be true")
	}
	if len(got.PrivacyModes) != 5 {
		t.Fatalf("expected 5 modes, got %v", got.PrivacyModes)
	}
}

func TestHandler_Capabilities_DefensiveCopyOfBackends(t *testing.T) {
	t.Parallel()
	src := []string{"vector"}
	h, _ := b2c.NewHandler(b2c.HandlerConfig{EnabledBackends: src})
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	r.ServeHTTP(w, req)
	src[0] = "tampered"
	var got b2c.Capabilities
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.EnabledBackends[0] != "vector" {
		t.Fatalf("response mutated by caller, got %v", got.EnabledBackends)
	}
}

func TestHandler_SyncSchedule_DefaultResolver(t *testing.T) {
	t.Parallel()
	h, _ := b2c.NewHandler(b2c.HandlerConfig{})
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/schedule", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got b2c.SyncSchedule
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TenantID != "tenant-a" {
		t.Fatalf("tenant_id=%q", got.TenantID)
	}
	if got.ForegroundSyncInterval != 60*time.Second {
		t.Fatalf("foreground=%v", got.ForegroundSyncInterval)
	}
	if got.BackgroundSyncInterval != 15*time.Minute {
		t.Fatalf("background=%v", got.BackgroundSyncInterval)
	}
	if got.JitterSeconds == 0 {
		t.Fatalf("jitter must be > 0")
	}
}

func TestHandler_SyncSchedule_CustomResolver(t *testing.T) {
	t.Parallel()
	resolver := b2c.StaticSyncResolver(func(tenantID string) b2c.SyncSchedule {
		return b2c.SyncSchedule{
			TenantID:                 tenantID,
			ForegroundSyncInterval:   30 * time.Second,
			BackgroundSyncInterval:   5 * time.Minute,
			MinForegroundSyncSeconds: 10,
			MinBackgroundSyncSeconds: 60,
			JitterSeconds:            5,
		}
	})
	h, _ := b2c.NewHandler(b2c.HandlerConfig{SyncResolver: resolver})
	r := newRouter(t, h, "tenant-b")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/schedule", nil)
	r.ServeHTTP(w, req)
	var got b2c.SyncSchedule
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.ForegroundSyncInterval != 30*time.Second {
		t.Fatalf("foreground=%v", got.ForegroundSyncInterval)
	}
	if got.JitterSeconds != 5 {
		t.Fatalf("jitter=%d", got.JitterSeconds)
	}
}

func TestHandler_SyncSchedule_RequiresTenantContext(t *testing.T) {
	t.Parallel()
	h, _ := b2c.NewHandler(b2c.HandlerConfig{})
	r := newRouter(t, h, "")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sync/schedule", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestStaticSyncResolver_NilFnFallsBackToDefault(t *testing.T) {
	t.Parallel()
	r := b2c.StaticSyncResolver(nil)
	got := r.Schedule("tenant-a")
	want := b2c.DefaultSyncSchedule("tenant-a")
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestDefaultSyncSchedule_FloorsAreNonZero(t *testing.T) {
	t.Parallel()
	got := b2c.DefaultSyncSchedule("tenant-a")
	if got.MinForegroundSyncSeconds <= 0 {
		t.Fatalf("min foreground floor must be > 0, got %d", got.MinForegroundSyncSeconds)
	}
	if got.MinBackgroundSyncSeconds <= 0 {
		t.Fatalf("min background floor must be > 0, got %d", got.MinBackgroundSyncSeconds)
	}
	if got.ForegroundSyncInterval < time.Duration(got.MinForegroundSyncSeconds)*time.Second {
		t.Fatalf("foreground interval %v < floor %ds", got.ForegroundSyncInterval, got.MinForegroundSyncSeconds)
	}
	if got.BackgroundSyncInterval < time.Duration(got.MinBackgroundSyncSeconds)*time.Second {
		t.Fatalf("background interval %v < floor %ds", got.BackgroundSyncInterval, got.MinBackgroundSyncSeconds)
	}
}

func TestHandler_Capabilities_EmptyBackendsRendersJSONArray(t *testing.T) {
	t.Parallel()
	h, _ := b2c.NewHandler(b2c.HandlerConfig{})
	r := newRouter(t, h, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, ok := got["enabled_backends"]
	if !ok {
		t.Fatalf("missing enabled_backends")
	}
	if _, isSlice := v.([]any); !isSlice {
		t.Fatalf("enabled_backends must be JSON array, got %T", v)
	}
}
