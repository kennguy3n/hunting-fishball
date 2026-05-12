// stage_breaker_handler.go — Round-14 Task 1.
//
// GET /v1/admin/pipeline/breakers exposes the per-stage circuit
// breaker dashboard. The handler is a thin adapter around the
// pipeline.StageBreakerInspector seam: production wires the
// process-wide StageBreakerRegistry, tests inject an in-memory
// slice.
//
// The endpoint is operator-facing — there is no per-tenant
// scoping on the response. Breaker state is cluster-global so an
// admin viewing it sees the same rows regardless of which tenant
// header was attached. This mirrors GET /v1/admin/health/summary
// and GET /v1/admin/health/indexes.
package admin

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// StageBreakerHandler serves the breakers dashboard.
type StageBreakerHandler struct {
	inspector pipeline.StageBreakerInspector
}

// NewStageBreakerHandler returns a handler bound to inspector.
// A nil inspector is tolerated — the endpoint then returns an
// empty list so a deployment with the breakers gate off still
// gets a sensible response.
func NewStageBreakerHandler(inspector pipeline.StageBreakerInspector) *StageBreakerHandler {
	return &StageBreakerHandler{inspector: inspector}
}

// Register mounts the endpoint on rg.
func (h *StageBreakerHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/pipeline/breakers", h.list)
}

// StageBreakerResponse is the JSON envelope.
type StageBreakerResponse struct {
	Items []pipeline.StageBreakerSnapshot `json:"items"`
}

func (h *StageBreakerHandler) list(c *gin.Context) {
	var rows []pipeline.StageBreakerSnapshot
	if h.inspector != nil {
		rows = h.inspector.Snapshot()
	}
	// Stable ordering — the pipeline's bounded enumeration of
	// stages (fetch/parse/embed/store) maps to a small alphabet
	// so a sort by stage name keeps the operator-facing view
	// deterministic across reloads.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Stage < rows[j].Stage
	})
	c.JSON(http.StatusOK, StageBreakerResponse{Items: rows})
}
