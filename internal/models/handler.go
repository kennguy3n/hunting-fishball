package models

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler serves GET /v1/models/catalog. Wired into cmd/api/main.go
// behind the same auth middleware as the rest of the API surface.
//
// The catalog itself is not tenant-scoped — it describes the
// universe of distributable model builds available to every
// device. Tenant-specific selection (e.g. an admin who has
// disabled fp16 for cost reasons) lives in the policy framework
// and is layered on top of the catalog client-side.
type Handler struct {
	provider Provider
}

// NewHandler builds a Handler around the supplied Provider.
func NewHandler(p Provider) (*Handler, error) {
	if p == nil {
		return nil, errors.New("models: nil Provider")
	}
	return &Handler{provider: p}, nil
}

// Register mounts GET /v1/models/catalog under rg.
func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/models/catalog", h.catalog)
}

func (h *Handler) catalog(c *gin.Context) {
	c.JSON(http.StatusOK, h.provider.Catalog())
}
