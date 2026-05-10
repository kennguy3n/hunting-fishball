// dashboard_handler.go — Phase 8 / Task 19 connector + retrieval
// health summary endpoint.
//
// Mounts GET /v1/admin/dashboard. The handler aggregates per-source
// connector health from the source_health table, optional pipeline
// throughput / retrieval latency snapshots from a metrics provider,
// and per-backend availability flags. The shape is deliberately
// loose (additionalProperties: true in OpenAPI) so the admin portal
// can iterate without breaking server changes.
package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// MetricsSnapshot is the optional metrics aggregator interface
// (Prometheus client wrappers, in-memory test fakes, etc.).
type MetricsSnapshot interface {
	// PipelineThroughputDocsPerMin returns the rolling docs/min rate
	// for the supplied tenant (or all tenants when "" is passed).
	PipelineThroughputDocsPerMin(ctx context.Context, tenantID string) (float64, error)
	// RetrievalP95Ms returns the rolling P95 retrieval latency for
	// the tenant (or all tenants when "" is passed).
	RetrievalP95Ms(ctx context.Context, tenantID string) (float64, error)
	// BackendAvailability reports per-backend up/down (vector,
	// bm25, graph, memory, kafka, etc.).
	BackendAvailability(ctx context.Context) (map[string]bool, error)
}

// DashboardHandler aggregates connector health, throughput, and
// availability into a single envelope.
type DashboardHandler struct {
	healths *HealthRepository
	metrics MetricsSnapshot
}

// NewDashboardHandler constructs the handler. metrics may be nil —
// the handler returns the connector portion without it.
func NewDashboardHandler(healths *HealthRepository, metrics MetricsSnapshot) (*DashboardHandler, error) {
	if healths == nil {
		return nil, errors.New("admin: nil health repo")
	}
	return &DashboardHandler{healths: healths, metrics: metrics}, nil
}

// Register mounts GET /v1/admin/dashboard on rg.
func (h *DashboardHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/dashboard", h.dashboard)
}

// DashboardConnector summarises one connector for the widget.
type DashboardConnector struct {
	SourceID   string       `json:"source_id"`
	Status     HealthStatus `json:"status"`
	Lag        int          `json:"lag"`
	ErrorCount int          `json:"error_count"`
}

// DashboardSummary is the top-level envelope.
type DashboardSummary struct {
	TenantID            string               `json:"tenant_id"`
	Connectors          []DashboardConnector `json:"connectors"`
	HealthyCount        int                  `json:"healthy_count"`
	DegradedCount       int                  `json:"degraded_count"`
	FailingCount        int                  `json:"failing_count"`
	UnknownCount        int                  `json:"unknown_count"`
	ThroughputDocsPMin  float64              `json:"throughput_docs_per_min,omitempty"`
	RetrievalP95Ms      float64              `json:"retrieval_p95_ms,omitempty"`
	BackendAvailability map[string]bool      `json:"backend_availability,omitempty"`
}

func (h *DashboardHandler) dashboard(c *gin.Context) {
	tid, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tid.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	rows, err := h.healths.ListByTenant(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list health failed"})
		return
	}
	out := DashboardSummary{TenantID: tenantID}
	for _, r := range rows {
		out.Connectors = append(out.Connectors, DashboardConnector{
			SourceID:   r.SourceID,
			Status:     r.Status,
			Lag:        r.Lag,
			ErrorCount: r.ErrorCount,
		})
		switch r.Status {
		case HealthStatusHealthy:
			out.HealthyCount++
		case HealthStatusDegraded:
			out.DegradedCount++
		case HealthStatusFailing:
			out.FailingCount++
		default:
			out.UnknownCount++
		}
	}
	if h.metrics != nil {
		ctx := c.Request.Context()
		if v, mErr := h.metrics.PipelineThroughputDocsPerMin(ctx, tenantID); mErr == nil {
			out.ThroughputDocsPMin = v
		}
		if v, mErr := h.metrics.RetrievalP95Ms(ctx, tenantID); mErr == nil {
			out.RetrievalP95Ms = v
		}
		if v, mErr := h.metrics.BackendAvailability(ctx); mErr == nil {
			out.BackendAvailability = v
		}
	}
	c.JSON(http.StatusOK, out)
}
