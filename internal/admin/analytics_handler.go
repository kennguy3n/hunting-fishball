// analytics_handler.go — Round-5 Task 19.
//
// GET /v1/admin/analytics/global is the super-admin-only
// cross-tenant dashboard endpoint. It aggregates:
//
//   - total_tenants         (distinct tenant_ids from sources)
//   - total_active_sources  (status=active sources)
//   - total_documents       (sum of sync-progress discovered)
//   - total_chunks          (from storage stats provider)
//   - per_backend_storage   (per-backend utilisation map)
//   - dlq_depth             (count of non-replayed DLQ messages)
//
// All numbers come from a narrow set of provider interfaces so
// the handler is pure business logic + JSON projection and the
// full persistence stack stays behind testable fakes.
//
// Gated by RoleSuperAdmin (Task 4 RBAC). Callers with admin or
// viewer roles get a 403.
package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AnalyticsSourceStats are the minimal source-aggregate stats.
type AnalyticsSourceStats struct {
	TotalTenants       int `json:"total_tenants"`
	TotalActiveSources int `json:"total_active_sources"`
}

// AnalyticsStorageStats captures per-backend utilisation.
type AnalyticsStorageStats struct {
	TotalChunks int64            `json:"total_chunks"`
	PerBackend  map[string]int64 `json:"per_backend_storage"`
}

// AnalyticsSourceProvider computes cross-tenant source stats.
type AnalyticsSourceProvider interface {
	GlobalSourceStats(ctx context.Context) (AnalyticsSourceStats, error)
}

// AnalyticsDocumentProvider gives the sum of all sync-progress
// discovered counts.
type AnalyticsDocumentProvider interface {
	TotalDocuments(ctx context.Context) (int64, error)
}

// AnalyticsStorageProvider gives chunk/backend stats.
type AnalyticsStorageProvider interface {
	GlobalStorageStats(ctx context.Context) (AnalyticsStorageStats, error)
}

// AnalyticsDLQProvider gives the current DLQ depth.
type AnalyticsDLQProvider interface {
	DLQDepth(ctx context.Context) (int64, error)
}

// AnalyticsConfig wires the analytics handler.
type AnalyticsConfig struct {
	Sources   AnalyticsSourceProvider
	Documents AnalyticsDocumentProvider
	Storage   AnalyticsStorageProvider
	DLQ       AnalyticsDLQProvider
}

// AnalyticsHandler serves GET /v1/admin/analytics/global.
type AnalyticsHandler struct {
	cfg AnalyticsConfig
}

// NewAnalyticsHandler validates cfg and returns the handler.
func NewAnalyticsHandler(cfg AnalyticsConfig) (*AnalyticsHandler, error) {
	if cfg.Sources == nil {
		return nil, errors.New("analytics: nil Sources")
	}
	if cfg.Documents == nil {
		return nil, errors.New("analytics: nil Documents")
	}
	if cfg.Storage == nil {
		return nil, errors.New("analytics: nil Storage")
	}
	if cfg.DLQ == nil {
		return nil, errors.New("analytics: nil DLQ")
	}
	return &AnalyticsHandler{cfg: cfg}, nil
}

// Register mounts the routes. Expects rg to be the /v1/admin
// group so the endpoint is /v1/admin/analytics/global.
func (h *AnalyticsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/analytics/global", RequireRole(RoleSuperAdmin), h.global)
}

// GlobalAnalyticsResponse is the JSON body.
type GlobalAnalyticsResponse struct {
	TotalTenants       int              `json:"total_tenants"`
	TotalActiveSources int              `json:"total_active_sources"`
	TotalDocuments     int64            `json:"total_documents"`
	TotalChunks        int64            `json:"total_chunks"`
	PerBackendStorage  map[string]int64 `json:"per_backend_storage"`
	DLQDepth           int64            `json:"dlq_depth"`
}

func (h *AnalyticsHandler) global(c *gin.Context) {
	ctx := c.Request.Context()
	srcStats, err := h.cfg.Sources.GlobalSourceStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	totalDocs, err := h.cfg.Documents.TotalDocuments(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	storageStats, err := h.cfg.Storage.GlobalStorageStats(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	dlqDepth, err := h.cfg.DLQ.DLQDepth(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, GlobalAnalyticsResponse{
		TotalTenants:       srcStats.TotalTenants,
		TotalActiveSources: srcStats.TotalActiveSources,
		TotalDocuments:     totalDocs,
		TotalChunks:        storageStats.TotalChunks,
		PerBackendStorage:  storageStats.PerBackend,
		DLQDepth:           dlqDepth,
	})
}
