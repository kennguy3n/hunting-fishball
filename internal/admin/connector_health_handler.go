// connector_health_handler.go — Round-20/21 Task 15.
//
// Mounts GET /v1/admin/connectors/health. Aggregates per-connector
// type health stats by joining sources × source_health rows: a row
// per connector_type with the count of healthy / degraded /
// failing / unknown / paused / removed sources, the average lag,
// the average error count, and the total error count. The admin
// portal renders this as a "fleet" view on top of the per-source
// dashboard at /v1/admin/dashboard.
//
// The handler reuses HealthRepository.ListByTenant + the existing
// SourceRepository.ListAllForTenant — no new DB queries. The
// aggregation is in-memory so a tenant with thousands of sources
// still completes in <100ms.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// ConnectorTypeHealth is one row of the aggregated dashboard.
type ConnectorTypeHealth struct {
	ConnectorType string `json:"connector_type"`
	Total         int    `json:"total"`
	Healthy       int    `json:"healthy"`
	Degraded      int    `json:"degraded"`
	Failing       int    `json:"failing"`
	Unknown       int    `json:"unknown"`
	Paused        int    `json:"paused"`
	Removing      int    `json:"removing"`
	Removed       int    `json:"removed"`
	// AvgLag is the arithmetic mean of source_health.lag across
	// the connector_type. Zero when no health rows exist.
	AvgLag float64 `json:"avg_lag"`
	// AvgErrorCount is the arithmetic mean of
	// source_health.error_count across the connector_type.
	AvgErrorCount float64 `json:"avg_error_count"`
	// ErrorRate is the fraction of sources whose status is
	// failing or degraded (0.0–1.0).
	ErrorRate float64 `json:"error_rate"`
}

// ConnectorHealthSummary is the top-level envelope returned by
// GET /v1/admin/connectors/health.
type ConnectorHealthSummary struct {
	TenantID   string                `json:"tenant_id"`
	Total      int                   `json:"total_sources"`
	Connectors []ConnectorTypeHealth `json:"connectors"`
}

// ConnectorHealthHandler aggregates per connector_type health for
// the requesting tenant.
type ConnectorHealthHandler struct {
	sources *SourceRepository
	healths *HealthRepository
}

// NewConnectorHealthHandler wires the handler. Both stores are
// required.
func NewConnectorHealthHandler(sources *SourceRepository, healths *HealthRepository) (*ConnectorHealthHandler, error) {
	if sources == nil {
		return nil, errors.New("admin: nil source repo")
	}
	if healths == nil {
		return nil, errors.New("admin: nil health repo")
	}
	return &ConnectorHealthHandler{sources: sources, healths: healths}, nil
}

// Register mounts GET /v1/admin/connectors/health on rg.
func (h *ConnectorHealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/connectors/health", h.connectorsHealth)
}

func (h *ConnectorHealthHandler) connectorsHealth(c *gin.Context) {
	tid, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tid.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	ctx := c.Request.Context()
	out, err := h.aggregate(ctx, tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}
	c.JSON(http.StatusOK, out)
}

// aggregate is exposed for unit tests — it returns the same
// payload the HTTP handler emits without going through gin.
func (h *ConnectorHealthHandler) aggregate(ctx context.Context, tenantID string) (ConnectorHealthSummary, error) {
	if tenantID == "" {
		return ConnectorHealthSummary{}, errors.New("admin: empty tenant")
	}
	srcs, err := h.sources.ListAllForTenant(ctx, tenantID)
	if err != nil {
		return ConnectorHealthSummary{}, err
	}
	rows, err := h.healths.ListByTenant(ctx, tenantID)
	if err != nil {
		return ConnectorHealthSummary{}, err
	}
	byID := make(map[string]Health, len(rows))
	for _, r := range rows {
		byID[r.SourceID] = r
	}
	type acc struct {
		row          ConnectorTypeHealth
		totalLag     int
		totalErrCnt  int
		healthRowCnt int
	}
	bucket := map[string]*acc{}
	for _, s := range srcs {
		a, ok := bucket[s.ConnectorType]
		if !ok {
			a = &acc{row: ConnectorTypeHealth{ConnectorType: s.ConnectorType}}
			bucket[s.ConnectorType] = a
		}
		a.row.Total++
		switch s.Status {
		case SourceStatusPaused:
			a.row.Paused++
		case SourceStatusRemoving:
			a.row.Removing++
		case SourceStatusRemoved:
			a.row.Removed++
		default:
			// active source — fall through to health bucket
		}
		hr, hasHealth := byID[s.ID]
		if !hasHealth {
			if s.Status == SourceStatusActive {
				a.row.Unknown++
			}

			continue
		}
		a.totalLag += hr.Lag
		a.totalErrCnt += hr.ErrorCount
		a.healthRowCnt++
		if s.Status != SourceStatusActive {
			continue
		}
		switch hr.Status {
		case HealthStatusHealthy:
			a.row.Healthy++
		case HealthStatusDegraded:
			a.row.Degraded++
		case HealthStatusFailing:
			a.row.Failing++
		default:
			a.row.Unknown++
		}
	}
	out := ConnectorHealthSummary{TenantID: tenantID, Total: len(srcs)}
	for _, a := range bucket {
		if a.healthRowCnt > 0 {
			a.row.AvgLag = float64(a.totalLag) / float64(a.healthRowCnt)
			a.row.AvgErrorCount = float64(a.totalErrCnt) / float64(a.healthRowCnt)
		}
		if a.row.Total > 0 {
			a.row.ErrorRate = float64(a.row.Failing+a.row.Degraded) / float64(a.row.Total)
		}
		out.Connectors = append(out.Connectors, a.row)
	}
	sort.Slice(out.Connectors, func(i, j int) bool {
		return out.Connectors[i].ConnectorType < out.Connectors[j].ConnectorType
	})

	return out, nil
}
