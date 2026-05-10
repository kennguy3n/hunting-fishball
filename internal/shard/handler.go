package shard

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// HandlerRepo is the narrow read surface the HTTP handler needs.
// Defining it as an interface keeps tests free of *gorm.DB.
type HandlerRepo interface {
	List(ctx context.Context, f ScopeFilter, pageSize int) ([]ShardManifest, error)
	GetByVersion(ctx context.Context, f ScopeFilter, version int64) (*ShardManifest, error)
	ChunkIDs(ctx context.Context, tenantID, shardID string) ([]string, error)
	LatestVersion(ctx context.Context, f ScopeFilter) (int64, error)
}

// ForgetTrigger is the seam the DELETE /v1/tenants/:tenant_id/keys
// handler invokes. The default implementation is the Forget
// orchestrator in forget.go; tests inject a fake.
type ForgetTrigger interface {
	Forget(ctx context.Context, tenantID, requestedBy string) error
}

// HandlerConfig configures a Handler.
type HandlerConfig struct {
	Repo   HandlerRepo
	Forget ForgetTrigger

	// CoverageRepo supplies the corpus-side chunk count for the
	// `/v1/shards/:tenant_id/coverage` endpoint. Optional: when
	// nil, the handler reports IsAuthoritative=false with reason
	// "corpus_size_unknown" instead of fabricating a denominator.
	CoverageRepo CoverageRepo

	// MaxPageSize bounds the list response. Defaults to 200.
	MaxPageSize int
}

// Handler serves the shard sync API surface.
type Handler struct {
	cfg HandlerConfig
}

// NewHandler validates cfg and returns a Handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Repo == nil {
		return nil, errors.New("shard: nil Repo")
	}
	if cfg.MaxPageSize <= 0 {
		cfg.MaxPageSize = 200
	}
	return &Handler{cfg: cfg}, nil
}

// Register mounts the shard sync endpoints under rg. Routes:
//
//	GET    /v1/shards/:tenant_id            — list manifests for the tenant scope
//	GET    /v1/shards/:tenant_id/delta      — delta against ?since=<version>
//	GET    /v1/shards/:tenant_id/coverage   — local-shard coverage metadata
//	DELETE /v1/tenants/:tenant_id/keys      — cryptographic forgetting trigger
//
// All four endpoints enforce the same tenant guard the rest of the
// API uses: the path-supplied tenant_id MUST match the
// authenticated tenant from the Gin context. A mismatch is returned
// as 403 to avoid leaking the existence of other tenants.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/shards/:tenant_id", h.list)
	rg.GET("/v1/shards/:tenant_id/delta", h.delta)
	rg.GET("/v1/shards/:tenant_id/coverage", h.coverage)
	if h.cfg.Forget != nil {
		rg.DELETE("/v1/tenants/:tenant_id/keys", h.forget)
	}
}

// listResponse is the JSON shape of GET /v1/shards/:tenant_id.
type listResponse struct {
	Shards []ShardManifest `json:"shards"`
}

func (h *Handler) list(c *gin.Context) {
	tenantID, ok := h.tenantFromPath(c)
	if !ok {
		return
	}

	pageSize := h.cfg.MaxPageSize
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > h.cfg.MaxPageSize {
				n = h.cfg.MaxPageSize
			}
			pageSize = n
		}
	}

	f := ScopeFilter{
		TenantID:    tenantID,
		UserID:      c.Query("user_id"),
		ChannelID:   c.Query("channel_id"),
		PrivacyMode: c.Query("privacy_mode"),
		Status:      ShardStatus(strings.ToLower(c.Query("status"))),
	}
	// Default to ready manifests so clients don't see in-flight
	// pending rows.
	if f.Status == "" {
		f.Status = ShardStatusReady
	}

	rows, err := h.cfg.Repo.List(c.Request.Context(), f, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, listResponse{Shards: rows})
}

// deltaResponse is the JSON shape of GET /v1/shards/:tenant_id/delta.
//
// Each operation describes a chunk-level change between the client's
// shard_version and the server's freshest one. Clients apply the
// adds first, then the removes, to land at the current version.
type deltaResponse struct {
	From        int64          `json:"from_version"`
	To          int64          `json:"to_version"`
	PrivacyMode string         `json:"privacy_mode"`
	Manifest    *ShardManifest `json:"manifest"`
	Operations  []DeltaOp      `json:"operations"`
	IsFullSync  bool           `json:"is_full_sync"`
}

func (h *Handler) delta(c *gin.Context) {
	tenantID, ok := h.tenantFromPath(c)
	if !ok {
		return
	}
	since, err := strconv.ParseInt(c.DefaultQuery("since", "0"), 10, 64)
	if err != nil || since < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since must be a non-negative integer"})
		return
	}
	privacyMode := c.Query("privacy_mode")
	if privacyMode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "privacy_mode is required"})
		return
	}

	scope := ScopeFilter{
		TenantID:    tenantID,
		UserID:      c.Query("user_id"),
		ChannelID:   c.Query("channel_id"),
		PrivacyMode: privacyMode,
	}
	latest, err := h.cfg.Repo.LatestVersion(c.Request.Context(), scope)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if latest == 0 {
		c.JSON(http.StatusOK, deltaResponse{From: since, To: 0, PrivacyMode: privacyMode})
		return
	}
	// Client is already at (or somehow ahead of) the freshest
	// manifest. Return an empty delta — emitting every chunk as an
	// add op would force the device to redo work it has already
	// completed and waste bandwidth.
	if since > 0 && since >= latest {
		c.JSON(http.StatusOK, deltaResponse{
			From:        since,
			To:          latest,
			PrivacyMode: privacyMode,
		})
		return
	}

	currManifest, err := h.cfg.Repo.GetByVersion(c.Request.Context(), scope, latest)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	currChunks, err := h.cfg.Repo.ChunkIDs(c.Request.Context(), tenantID, currManifest.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var prevChunks []string
	isFullSync := since == 0
	if since > 0 && since < latest {
		prevManifest, err := h.cfg.Repo.GetByVersion(c.Request.Context(), scope, since)
		if err != nil && !errors.Is(err, ErrShardNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err == nil {
			prevChunks, err = h.cfg.Repo.ChunkIDs(c.Request.Context(), tenantID, prevManifest.ID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		} else {
			// Client claims a version we don't have on file; fall
			// back to a full sync so they don't get stuck.
			isFullSync = true
		}
	}

	ops := ComputeDelta(prevChunks, currChunks)
	c.JSON(http.StatusOK, deltaResponse{
		From:        since,
		To:          latest,
		PrivacyMode: privacyMode,
		Manifest:    currManifest,
		Operations:  ops,
		IsFullSync:  isFullSync,
	})
}

// CoverageResponse is the JSON shape of
// GET /v1/shards/:tenant_id/coverage. Documents how much of the
// tenant's corpus is reachable from the on-device shard so the
// client's local-first decision tree (see
// docs/contracts/local-first-retrieval.md) can choose between
// local and remote retrieval without re-deriving counts on every
// query.
type CoverageResponse struct {
	TenantID        string  `json:"tenant_id"`
	PrivacyMode     string  `json:"privacy_mode"`
	ShardVersion    int64   `json:"shard_version"`
	ShardChunks     int     `json:"shard_chunks"`
	CorpusChunks    int     `json:"corpus_chunks"`
	CoverageRatio   float64 `json:"coverage_ratio"`
	IsAuthoritative bool    `json:"is_authoritative"`
	Reason          string  `json:"reason,omitempty"`
}

// CoverageRepo is the read used by
// GET /v1/shards/:tenant_id/coverage. Implementations resolve the
// total chunk count for the tenant scope (the denominator of the
// coverage ratio). Optional — when no CoverageRepo is wired the
// handler reports a coverage ratio of 0 and IsAuthoritative=false
// so the client falls back to remote retrieval.
type CoverageRepo interface {
	CorpusChunkCount(ctx context.Context, f ScopeFilter) (int, error)
}

func (h *Handler) coverage(c *gin.Context) {
	tenantID, ok := h.tenantFromPath(c)
	if !ok {
		return
	}
	privacyMode := c.Query("privacy_mode")
	if privacyMode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "privacy_mode is required"})
		return
	}
	scope := ScopeFilter{
		TenantID:    tenantID,
		UserID:      c.Query("user_id"),
		ChannelID:   c.Query("channel_id"),
		PrivacyMode: privacyMode,
	}
	latest, err := h.cfg.Repo.LatestVersion(c.Request.Context(), scope)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp := CoverageResponse{
		TenantID:    tenantID,
		PrivacyMode: privacyMode,
	}
	if latest == 0 {
		resp.Reason = "no_shard"
		c.JSON(http.StatusOK, resp)
		return
	}
	manifest, err := h.cfg.Repo.GetByVersion(c.Request.Context(), scope, latest)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp.ShardVersion = manifest.ShardVersion
	resp.ShardChunks = manifest.ChunksCount

	corpusRepo := h.cfg.CoverageRepo
	if corpusRepo == nil {
		// Without an explicit CoverageRepo wired in, the corpus
		// denominator is unknowable; surface that honestly rather
		// than emitting a fabricated 1.0 ratio.
		resp.Reason = "corpus_size_unknown"
		c.JSON(http.StatusOK, resp)
		return
	}
	corpus, err := corpusRepo.CorpusChunkCount(c.Request.Context(), scope)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp.CorpusChunks = corpus
	if corpus > 0 {
		resp.CoverageRatio = float64(manifest.ChunksCount) / float64(corpus)
		if resp.CoverageRatio > 1 {
			resp.CoverageRatio = 1
		}
		resp.IsAuthoritative = true
	}
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) forget(c *gin.Context) {
	tenantID, ok := h.tenantFromPath(c)
	if !ok {
		return
	}
	actor, _ := c.Get(audit.ActorContextKey)
	actorID, _ := actor.(string)
	if err := h.cfg.Forget.Forget(c.Request.Context(), tenantID, actorID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Forget runs the full workflow synchronously (mark pending →
	// drain → sweep → mark deleted). On a nil error every tier has
	// been swept and the lifecycle row is now `deleted`, so that's
	// the state we surface to the caller.
	c.JSON(http.StatusAccepted, gin.H{"tenant_id": tenantID, "state": string(LifecycleDeleted)})
}

// tenantFromPath verifies the path-supplied tenant matches the
// authenticated tenant from the Gin context. Returns false (after
// writing the response) on any mismatch / missing context.
func (h *Handler) tenantFromPath(c *gin.Context) (string, bool) {
	pathTenant := c.Param("tenant_id")
	if pathTenant == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing tenant_id"})
		return "", false
	}
	authVal, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return "", false
	}
	authTenant, _ := authVal.(string)
	if authTenant == "" || authTenant != pathTenant {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant mismatch"})
		return "", false
	}
	return pathTenant, true
}
