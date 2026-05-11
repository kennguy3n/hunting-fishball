package admin

// ab_test_handler.go — Round-6 Task 10.
//
// Admin endpoints for retrieval A/B experiments:
//
//   POST /v1/admin/retrieval/experiments    — create or update.
//   GET  /v1/admin/retrieval/experiments    — list for tenant.
//   GET  /v1/admin/retrieval/experiments/:n — get one.
//   DELETE /v1/admin/retrieval/experiments/:n — remove.

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// ABTestHandler exposes the retrieval A/B-test admin surface.
type ABTestHandler struct {
	store ABTestStore
	audit AuditWriter
}

// NewABTestHandler validates and constructs the handler.
func NewABTestHandler(store ABTestStore, auditWriter AuditWriter) (*ABTestHandler, error) {
	if store == nil {
		return nil, errors.New("ab handler: nil store")
	}
	return &ABTestHandler{store: store, audit: auditWriter}, nil
}

// Register attaches routes.
func (h *ABTestHandler) Register(g *gin.RouterGroup) {
	g.POST("/v1/admin/retrieval/experiments", h.upsert)
	g.GET("/v1/admin/retrieval/experiments", h.list)
	g.GET("/v1/admin/retrieval/experiments/:name", h.get)
	g.DELETE("/v1/admin/retrieval/experiments/:name", h.delete)
}

func (h *ABTestHandler) tenantOrAbort(c *gin.Context) (string, bool) {
	tID, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tID.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return "", false
	}
	return tenantID, true
}

func (h *ABTestHandler) upsert(c *gin.Context) {
	tenantID, ok := h.tenantOrAbort(c)
	if !ok {
		return
	}
	var body ABTestConfig
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body.TenantID = tenantID
	if err := h.store.Upsert(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.audit != nil {
		actor, _ := c.Get(audit.ActorContextKey)
		actorID, _ := actor.(string)
		_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
			tenantID, actorID,
			audit.ActionPolicyEdited,
			"retrieval-experiment", body.ExperimentName,
			audit.JSONMap{"status": string(body.Status), "split": body.TrafficSplitPercent},
			"",
		))
	}
	c.JSON(http.StatusOK, body)
}

func (h *ABTestHandler) list(c *gin.Context) {
	tenantID, ok := h.tenantOrAbort(c)
	if !ok {
		return
	}
	rows, err := h.store.List(tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"experiments": rows})
}

func (h *ABTestHandler) get(c *gin.Context) {
	tenantID, ok := h.tenantOrAbort(c)
	if !ok {
		return
	}
	name := c.Param("name")
	cfg, err := h.store.Get(tenantID, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

func (h *ABTestHandler) delete(c *gin.Context) {
	tenantID, ok := h.tenantOrAbort(c)
	if !ok {
		return
	}
	name := c.Param("name")
	if err := h.store.Delete(tenantID, name); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}
