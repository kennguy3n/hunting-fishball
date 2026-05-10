//go:build e2e

// Phase 6 B2C e2e surfaces. Exercises the three GET endpoints the
// per-platform B2C SDKs (Android / iOS / desktop) hit on cold start:
// `/v1/health`, `/v1/capabilities`, `/v1/sync/schedule`. These are
// the only endpoints below the auth boundary (or, in production,
// behind a thin API key) so cataloguing their JSON shape protects
// the SDKs from accidental contract drift.
package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/b2c"
)

// TestSmoke_B2CHealth asserts the health endpoint returns
// status=ok with a populated server time. The desktop SDK
// (uneycom/ai-agent-desktop) treats any non-200 as a hard failure
// and falls back to local-only mode; a stale `time` field would
// look like a clock-skew bug to a user, not a server outage.
func TestSmoke_B2CHealth(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	h, err := b2c.NewHandler(b2c.HandlerConfig{ServerVersion: "smoke"})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, uniqueTenant(t))
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/health", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp b2c.HealthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status=%q want ok", resp.Status)
	}
	if resp.Time.IsZero() {
		t.Errorf("time field empty")
	}
	if resp.Version != "smoke" {
		t.Errorf("version=%q want smoke", resp.Version)
	}
}

// TestSmoke_B2CCapabilities exercises the capabilities surface
// with the same flags the production cmd/api wiring sets.
// Validates EnabledBackends is non-empty (a client with zero
// enabled backends has no useful retrieval surface) and the
// PrivacyModes ladder contains every documented mode.
func TestSmoke_B2CCapabilities(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	h, err := b2c.NewHandler(b2c.HandlerConfig{
		ServerVersion:   "smoke",
		EnabledBackends: []string{"vector", "bm25"},
		LocalShardSync:  true,
		DeviceFirst:     true,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, uniqueTenant(t))
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/capabilities", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp b2c.Capabilities
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.EnabledBackends) == 0 {
		t.Errorf("enabled_backends empty")
	}
	wantModes := []string{"no-ai", "local-only", "local-api", "hybrid", "remote"}
	if len(resp.PrivacyModes) != len(wantModes) {
		t.Fatalf("privacy_modes=%v want exactly %v", resp.PrivacyModes, wantModes)
	}
	have := strings.Join(resp.PrivacyModes, ",")
	for _, want := range wantModes {
		if !strings.Contains(have, want) {
			t.Errorf("privacy_modes %q missing %q", have, want)
		}
	}
	if !resp.PrivacyStrip {
		t.Errorf("privacy_strip should be true (server invariant)")
	}
}

// TestSmoke_B2CSyncSchedule asserts the sync schedule's intervals
// are positive and the floor minimums are at least 30 s / 5 min —
// matching the documented DefaultSyncSchedule contract. The floors
// exist to keep a misbehaving client from polling the API every
// second; tightening them here without bumping the platform
// SDK is a regression.
func TestSmoke_B2CSyncSchedule(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	h, err := b2c.NewHandler(b2c.HandlerConfig{ServerVersion: "smoke"})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, uniqueTenant(t))
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/sync/schedule", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp b2c.SyncSchedule
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ForegroundSyncInterval <= 0 {
		t.Errorf("foreground_sync_interval=%s must be positive", resp.ForegroundSyncInterval)
	}
	if resp.BackgroundSyncInterval <= 0 {
		t.Errorf("background_sync_interval=%s must be positive", resp.BackgroundSyncInterval)
	}
	if resp.MinForegroundSyncSeconds < 30 {
		t.Errorf("min_foreground_sync_seconds=%d must be >= 30", resp.MinForegroundSyncSeconds)
	}
	if resp.MinBackgroundSyncSeconds < 5*60 {
		t.Errorf("min_background_sync_seconds=%d must be >= 300", resp.MinBackgroundSyncSeconds)
	}
	// Sanity: the foreground interval must be shorter than the
	// background one. Otherwise the schedule is incoherent.
	if resp.ForegroundSyncInterval >= resp.BackgroundSyncInterval {
		t.Errorf("foreground (%s) must be < background (%s)", resp.ForegroundSyncInterval, resp.BackgroundSyncInterval)
	}
	// Schedules without jitter would stampede the server on
	// shared wall-clock boundaries.
	if resp.JitterSeconds <= 0 {
		t.Errorf("jitter_seconds=%d must be > 0", resp.JitterSeconds)
	}
	_ = time.Now()
}
