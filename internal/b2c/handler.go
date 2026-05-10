// Package b2c serves the small set of "client SDK" endpoints the
// B2C mobile / desktop apps consume to bootstrap a session against
// the context engine. Each endpoint is intentionally cheap so it
// can run from a cold-start splash screen without delaying the
// first user-facing render.
//
// Endpoints:
//
//	GET /v1/health         - liveness ping for B2C clients
//	GET /v1/capabilities   - which retrieval backends + privacy modes are enabled
//	GET /v1/sync/schedule  - recommended shard sync intervals per tenant
//
// The endpoints are tenant-aware: the auth middleware populates
// the tenant_id; the handlers consult per-tenant config when the
// answer can vary across tenants. See
// `docs/contracts/b2c-retrieval-sdk.md` and
// `docs/contracts/background-sync.md` for the full client
// contract.
package b2c

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// HealthResponse is the JSON shape of GET /v1/health. Returned
// unconditionally with a 200 when the API process is up; the
// dependency-aware probe is /readyz.
type HealthResponse struct {
	Status  string    `json:"status"`
	Time    time.Time `json:"time"`
	Version string    `json:"version,omitempty"`
}

// Capabilities describes the server-side feature surface the B2C
// clients can rely on. Returned by GET /v1/capabilities so a
// client can adapt its UI (e.g. hide the "graph traversal" toggle
// when the graph backend is disabled).
type Capabilities struct {
	// EnabledBackends lists the retrieval backends the server
	// will fan out to. One of "vector" / "bm25" / "graph" /
	// "memory". A backend is "enabled" when the cmd/api wiring
	// constructed a non-nil client for it.
	EnabledBackends []string `json:"enabled_backends"`

	// PrivacyModes lists the privacy modes the server understands
	// in JSON ladder order ("no-ai", "local-only", "local-api",
	// "hybrid", "remote"). Clients render the modes their channel
	// policy permits from this list.
	PrivacyModes []string `json:"privacy_modes"`

	// LocalShardSync is true when the server can return shard
	// manifests for on-device retrieval (Phase 5).
	LocalShardSync bool `json:"local_shard_sync"`

	// PrivacyStrip is true when every retrieval row carries the
	// structured `privacy_strip` field. Phase 4 invariant.
	PrivacyStrip bool `json:"privacy_strip"`

	// DeviceFirst is true when the server emits the
	// `prefer_local` + `local_shard_version` hint in
	// RetrieveResponse. Phase 6 / Task 15.
	DeviceFirst bool `json:"device_first"`

	// ServerVersion is the build version surfaced to clients.
	ServerVersion string `json:"server_version,omitempty"`
}

// SyncSchedule is the JSON shape of GET /v1/sync/schedule. The
// schedule maps a request scope to the recommended sync interval
// the platform's native scheduler should configure. Clients
// re-fetch the schedule on every cold start so the server can
// re-tune intervals (e.g. shorten foreground sync when a heavy
// reindex is running) without a client release.
type SyncSchedule struct {
	TenantID                 string        `json:"tenant_id"`
	ForegroundSyncInterval   time.Duration `json:"foreground_sync_interval_ns"`
	BackgroundSyncInterval   time.Duration `json:"background_sync_interval_ns"`
	MinForegroundSyncSeconds int           `json:"min_foreground_sync_seconds"`
	MinBackgroundSyncSeconds int           `json:"min_background_sync_seconds"`
	JitterSeconds            int           `json:"jitter_seconds"`
}

// HandlerConfig configures the B2C Handler. ServerVersion is
// stamped onto every Health and Capabilities response. The
// per-handler boolean flags reflect the server's wired backends
// at construction time and never flip during the process
// lifetime.
type HandlerConfig struct {
	ServerVersion string

	// EnabledBackends is the list passed straight into the
	// Capabilities response. The cmd/api wiring computes the list
	// once at startup based on which backend constructors were
	// successful.
	EnabledBackends []string

	// LocalShardSync mirrors whether the shard handler is mounted.
	LocalShardSync bool

	// DeviceFirst mirrors whether the retrieval handler has a
	// ShardVersionLookup wired.
	DeviceFirst bool

	// SyncResolver returns the recommended sync schedule for the
	// supplied tenant. nil → DefaultSyncSchedule.
	SyncResolver SyncResolver
}

// SyncResolver is the per-tenant sync-interval port. The default
// implementation returns the server-wide DefaultSyncSchedule so a
// fresh tenant gets sensible defaults; the production wiring may
// look up tenant-specific overrides (e.g. high-volume tenants get a
// shorter foreground interval).
type SyncResolver interface {
	Schedule(tenantID string) SyncSchedule
}

// DefaultSyncSchedule is the platform-wide baseline:
//   - foreground (app open) sync every 60 s
//   - background (BGAppRefreshTask / WorkManager / powerMonitor)
//     sync every 15 min
//   - jitter ±30 s so a flock of clients on the same wall-clock
//     boundary doesn't stampede the API
//
// The minimums are floors a client MUST NOT undershoot — they exist
// so a rogue app build can't DoS the API by polling every second.
func DefaultSyncSchedule(tenantID string) SyncSchedule {
	return SyncSchedule{
		TenantID:                 tenantID,
		ForegroundSyncInterval:   60 * time.Second,
		BackgroundSyncInterval:   15 * time.Minute,
		MinForegroundSyncSeconds: 30,
		MinBackgroundSyncSeconds: 5 * 60,
		JitterSeconds:            30,
	}
}

// staticSyncResolver wraps a single SyncSchedule so cmd/api can
// register a process-wide resolver without writing a struct.
type staticSyncResolver struct{ fn func(string) SyncSchedule }

func (s staticSyncResolver) Schedule(tenantID string) SyncSchedule {
	return s.fn(tenantID)
}

// StaticSyncResolver wraps fn so it implements SyncResolver. fn is
// expected to return a SyncSchedule for the supplied tenant id.
func StaticSyncResolver(fn func(string) SyncSchedule) SyncResolver {
	if fn == nil {
		return staticSyncResolver{fn: DefaultSyncSchedule}
	}
	return staticSyncResolver{fn: fn}
}

// Handler serves the B2C client SDK endpoints.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates cfg and returns a Handler. The PrivacyStrip
// flag is hard-coded to true because the Phase 4 privacy strip is
// a server invariant, not a configurable backend.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.SyncResolver == nil {
		cfg.SyncResolver = StaticSyncResolver(DefaultSyncSchedule)
	}
	return &Handler{cfg: cfg}, nil
}

// Register mounts the B2C endpoints under rg.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/health", h.health)
	rg.GET("/v1/capabilities", h.capabilities)
	rg.GET("/v1/sync/schedule", h.syncSchedule)
}

func (h *Handler) health(c *gin.Context) {
	c.JSON(http.StatusOK, HealthResponse{
		Status:  "ok",
		Time:    time.Now().UTC(),
		Version: h.cfg.ServerVersion,
	})
}

func (h *Handler) capabilities(c *gin.Context) {
	c.JSON(http.StatusOK, Capabilities{
		EnabledBackends: append([]string(nil), h.cfg.EnabledBackends...),
		PrivacyModes: []string{
			"no-ai",
			"local-only",
			"local-api",
			"hybrid",
			"remote",
		},
		LocalShardSync: h.cfg.LocalShardSync,
		PrivacyStrip:   true,
		DeviceFirst:    h.cfg.DeviceFirst,
		ServerVersion:  h.cfg.ServerVersion,
	})
}

func (h *Handler) syncSchedule(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	c.JSON(http.StatusOK, h.cfg.SyncResolver.Schedule(tenantID))
}

// tenantIDFromContext reads the tenant id auth middleware stored
// on the gin context. Returns false when the context key is
// missing or empty.
func tenantIDFromContext(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
