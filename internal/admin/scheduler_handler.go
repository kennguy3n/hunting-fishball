package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SchedulerHandler serves the /v1/admin/sources/:id/schedule
// endpoints. The handler depends only on a *gorm.DB so it can be
// wired alongside the existing SourceHandler without restructuring
// the constructor.
type SchedulerHandler struct {
	db  *gorm.DB
	now func() time.Time
}

// NewSchedulerHandler returns a handler that reads/writes the
// SyncSchedule table.
func NewSchedulerHandler(db *gorm.DB) *SchedulerHandler {
	return &SchedulerHandler{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// Register mounts the schedule endpoints on the provided
// /v1/admin/sources router group. The caller controls the parent
// group's auth middleware.
func (h *SchedulerHandler) Register(rg *gin.RouterGroup) {
	g := rg.Group("/v1/admin/sources/:id/schedule")
	g.GET("", h.get)
	g.POST("", h.upsert)
	g.DELETE("", h.delete)
}

// AutoMigrate creates the sync_schedules table if it does not yet
// exist. Used by tests; production goes through migrations/.
func (h *SchedulerHandler) AutoMigrate(c *gin.Context) error {
	return h.db.WithContext(c.Request.Context()).AutoMigrate(&SyncSchedule{})
}

type scheduleRequest struct {
	CronExpr string `json:"cron_expr"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

func (h *SchedulerHandler) upsert(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	if sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source id required"})
		return
	}
	var req scheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row, err := UpsertSchedule(c.Request.Context(), h.db, tenantID, sourceID, req.CronExpr, enabled, h.now())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

func (h *SchedulerHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	row, err := GetSchedule(c.Request.Context(), h.db, tenantID, sourceID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, row)
}

func (h *SchedulerHandler) delete(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	tx := h.db.WithContext(c.Request.Context()).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		Delete(&SyncSchedule{})
	if tx.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": tx.Error.Error()})
		return
	}
	if tx.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "schedule not found"})
		return
	}
	c.Status(http.StatusNoContent)
}
