// sync_progress_handler.go — Phase 8 / Task 14 admin progress endpoint.
package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SyncProgressHandler serves GET /v1/admin/sources/:id/progress.
type SyncProgressHandler struct {
	store SyncProgressStore
}

// NewSyncProgressHandler wires the handler to the supplied store.
func NewSyncProgressHandler(store SyncProgressStore) *SyncProgressHandler {
	return &SyncProgressHandler{store: store}
}

// Register mounts GET /v1/admin/sources/:id/progress on rg.
func (h *SyncProgressHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/sources/:id/progress", h.get)
}

// SyncProgressItem is the per-namespace progress envelope.
type SyncProgressItem struct {
	NamespaceID string  `json:"namespace_id"`
	Discovered  int64   `json:"discovered"`
	Processed   int64   `json:"processed"`
	Failed      int64   `json:"failed"`
	PercentDone float64 `json:"percent_done"`
}

// SyncProgressResponse is the JSON envelope for the endpoint.
type SyncProgressResponse struct {
	TenantID string             `json:"tenant_id"`
	SourceID string             `json:"source_id"`
	Items    []SyncProgressItem `json:"items"`
}

func (h *SyncProgressHandler) get(c *gin.Context) {
	tenantID, _ := c.Get(audit.TenantContextKey)
	tid, _ := tenantID.(string)
	if tid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	rows, err := h.store.List(c.Request.Context(), tid, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	resp := SyncProgressResponse{TenantID: tid, SourceID: id}
	for _, r := range rows {
		percent := 0.0
		if r.Discovered > 0 {
			percent = float64(r.Processed) / float64(r.Discovered) * 100
			if percent > 100 {
				percent = 100
			}
		}
		resp.Items = append(resp.Items, SyncProgressItem{
			NamespaceID: r.NamespaceID,
			Discovered:  r.Discovered,
			Processed:   r.Processed,
			Failed:      r.Failed,
			PercentDone: percent,
		})
	}
	c.JSON(http.StatusOK, resp)
}
