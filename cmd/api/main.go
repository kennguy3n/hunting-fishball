// Command context-engine-api is the Gin retrieval API binary. The Phase
// 0 build is a stub: it imports the audit-log handler so the routes
// surface compiles, but does not yet wire a *gorm.DB.
//
// Real wiring (Postgres connection, retrieval routes, OIDC middleware)
// lands in Phase 1.
package main

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
)

func main() {
	fmt.Fprintln(os.Stderr, "context-engine-api: phase-0 stub")

	r := gin.New()
	r.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })

	// Mount points are reserved here. Phase 1 will wire real handlers.
	_ = r
}
