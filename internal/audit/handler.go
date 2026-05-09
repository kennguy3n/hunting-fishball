package audit

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// TenantContextKey is the Gin context key auth middleware writes the
// authenticated tenant ID under. Handlers MUST pull tenant_id from this
// key and never from query params, request body, or path.
const TenantContextKey = "tenant_id"

// ActorContextKey is the Gin context key auth middleware writes the
// authenticated actor ID under.
const ActorContextKey = "actor_id"

// repoReader is the read surface the Gin handler needs from Repository.
// Defining it as an interface lets the handler tests use a fake without
// pulling in a real *gorm.DB.
type repoReader interface {
	List(ctx ginContext, f ListFilter) (*ListResult, error)
	Get(ctx ginContext, tenantID, id string) (*AuditLog, error)
}

// ginContext is an alias so the interface stays in this package without
// dragging the entire context package signature into the type. The
// concrete *Repository methods accept context.Context, which a
// *gin.Context satisfies via Request.Context().
type ginContext = ctxAlias

// readerAdapter adapts *Repository to repoReader's signature, which uses
// context.Context. The handler always passes c.Request.Context().
type readerAdapter struct {
	r *Repository
}

func (a readerAdapter) List(ctx ginContext, f ListFilter) (*ListResult, error) {
	return a.r.List(ctx, f)
}

func (a readerAdapter) Get(ctx ginContext, tenantID, id string) (*AuditLog, error) {
	return a.r.Get(ctx, tenantID, id)
}

// Handler serves the audit-log HTTP API surface used by the admin portal.
// All handlers enforce tenant isolation: tenant_id comes from the Gin
// request context (populated by auth middleware) and is never read from
// the request itself.
type Handler struct {
	reader repoReader
}

// NewHandler wires a Handler to the supplied Repository.
func NewHandler(repo *Repository) *Handler {
	return &Handler{reader: readerAdapter{r: repo}}
}

// newHandlerWithReader is for tests.
func newHandlerWithReader(r repoReader) *Handler {
	return &Handler{reader: r}
}

// Register mounts the audit-log endpoints on rg. Routes:
//
//	GET /v1/audit-logs       — list, filtered + paginated
//	GET /v1/audit-logs/:id   — fetch a single entry
func (h *Handler) Register(rg *gin.RouterGroup) {
	g := rg.Group("/v1/audit-logs")
	g.GET("", h.list)
	g.GET("/:id", h.get)
}

// list serves GET /v1/audit-logs.
func (h *Handler) list(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}

	filter := ListFilter{TenantID: tenantID}

	if a := c.QueryArray("action"); len(a) > 0 {
		filter.Actions = make([]Action, 0, len(a))
		for _, v := range a {
			filter.Actions = append(filter.Actions, Action(v))
		}
	}
	if rt := c.Query("resource_type"); rt != "" {
		filter.ResourceType = rt
	}
	if s := c.Query("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})

			return
		}
		filter.Since = t
	}
	if u := c.Query("until"); u != "" {
		t, err := time.Parse(time.RFC3339, u)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "until must be RFC3339"})

			return
		}
		filter.Until = t
	}
	if ps := c.Query("page_size"); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be a positive integer"})

			return
		}
		filter.PageSize = n
	}
	filter.PageToken = c.Query("page_token")

	res, err := h.reader.List(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})

		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items":           res.Items,
		"next_page_token": res.NextPageToken,
	})
}

// get serves GET /v1/audit-logs/:id.
func (h *Handler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})

		return
	}

	log, err := h.reader.Get(c.Request.Context(), tenantID, id)
	if errors.Is(err, ErrAuditLogNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})

		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "get failed"})

		return
	}

	c.JSON(http.StatusOK, log)
}

// tenantIDFromContext pulls the authenticated tenant ID out of the Gin
// context. The signature returns (string, bool) so callers don't have to
// distinguish missing from empty.
func tenantIDFromContext(c *gin.Context) (string, bool) {
	v, ok := c.Get(TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}

	return s, true
}
