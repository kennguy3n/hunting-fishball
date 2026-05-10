// Package observability — middleware glues Prometheus to Gin.
package observability

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// PrometheusMiddleware records request count + duration for every
// request served through the engine. Path is taken from
// FullPath() so high-cardinality concrete paths (with IDs etc.)
// don't leak into label space.
//
// Mount it BEFORE route registration:
//
//	r := gin.New()
//	r.Use(observability.PrometheusMiddleware())
func PrometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			path = "unmatched"
		}
		method := c.Request.Method
		status := strconv.Itoa(c.Writer.Status())
		APIRequestsTotal.WithLabelValues(method, path, status).Inc()
		APIRequestDurationSeconds.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	}
}
