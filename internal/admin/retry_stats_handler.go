package admin

// retry_stats_handler.go — Round-6 Task 19.
//
// Exposes the pipeline retry analytics over HTTP:
//
//	GET /v1/admin/pipeline/retry-stats

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// RetryStatsSource is the narrow surface the handler depends on.
// *pipeline.RetryAnalytics satisfies it.
type RetryStatsSource interface {
	Snapshot() []*pipeline.StageRetryStats
}

// RetryStatsHandler exposes the snapshot endpoint.
type RetryStatsHandler struct {
	src RetryStatsSource
}

// NewRetryStatsHandler validates and constructs the handler.
func NewRetryStatsHandler(src RetryStatsSource) (*RetryStatsHandler, error) {
	if src == nil {
		return nil, errors.New("retry stats handler: nil source")
	}
	return &RetryStatsHandler{src: src}, nil
}

// Register attaches the route.
func (h *RetryStatsHandler) Register(g *gin.RouterGroup) {
	g.GET("/v1/admin/pipeline/retry-stats", h.get)
}

func (h *RetryStatsHandler) get(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"stages": h.src.Snapshot()})
}
