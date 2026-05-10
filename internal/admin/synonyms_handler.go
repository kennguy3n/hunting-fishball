// synonyms_handler.go — Round-6 Task 4 admin surface.
//
//	GET  /v1/admin/synonyms   — return the tenant's synonym map
//	POST /v1/admin/synonyms   — replace the tenant's synonym map
//
// Synonyms are used by the retrieval handler's QueryExpander before
// fan-out (internal/retrieval/query_expander.go). The store is
// pluggable; the production wiring uses an in-memory store
// (NewInMemorySynonymStore) in the API binary so the configuration
// survives only as long as the process — admins repush on rollout.
// Tenants with thousands of synonyms should switch to a Redis-
// backed store; the in-memory implementation is the default.

package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SynonymStore is the narrow contract the admin handler uses. The
// retrieval package owns the canonical interface; we redeclare a
// matching shape here to avoid an import cycle.
type SynonymStore interface {
	Get(ctx context.Context, tenantID string) (map[string][]string, error)
	Set(ctx context.Context, tenantID string, synonyms map[string][]string) error
}

// SynonymsHandler serves the admin synonyms surface.
type SynonymsHandler struct {
	store SynonymStore
	audit AuditWriter
}

// NewSynonymsHandler validates inputs.
func NewSynonymsHandler(store SynonymStore, aw AuditWriter) (*SynonymsHandler, error) {
	if store == nil {
		return nil, errors.New("admin: nil synonym store")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &SynonymsHandler{store: store, audit: aw}, nil
}

// Register mounts the routes.
func (h *SynonymsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/synonyms", h.list)
	rg.POST("/v1/admin/synonyms", h.set)
}

type synonymsBody struct {
	Synonyms map[string][]string `json:"synonyms" binding:"required"`
}

func (h *SynonymsHandler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	out, err := h.store.Get(c.Request.Context(), tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if out == nil {
		out = map[string][]string{}
	}
	c.JSON(http.StatusOK, gin.H{"synonyms": out})
}

func (h *SynonymsHandler) set(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var body synonymsBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.Set(c.Request.Context(), tenantID, body.Synonyms); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorID := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionPolicyEdited, "synonyms", tenantID,
		audit.JSONMap{"keys": len(body.Synonyms)}, "",
	))
	c.JSON(http.StatusOK, gin.H{"keys": len(body.Synonyms)})
}
