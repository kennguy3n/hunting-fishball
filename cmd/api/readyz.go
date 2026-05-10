// readyz.go — Phase 8 Task 20: Kubernetes-style readiness probe for
// the API binary. The /healthz handler is a liveness probe (always
// 200 if the process is alive), while /readyz pings every external
// dependency: Postgres, Redis, and Qdrant.
//
// The probe returns 200 with a JSON body listing each dependency's
// state if all checks succeed; otherwise it returns 503 with the
// same envelope so an orchestrator can pull the pod out of rotation.
package main

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// apiReadyzHandler returns a Gin handler that pings each dependency
// in parallel-ish (sequentially, but each with its own short
// per-dependency deadline so a stuck connection can't poison the
// probe).
func apiReadyzHandler(db *gorm.DB, rc *redis.Client, qdrant *storage.QdrantClient) gin.HandlerFunc {
	return func(c *gin.Context) {
		states := map[string]string{}
		ok := true

		// Postgres
		if err := pingPostgres(c.Request.Context(), db); err != nil {
			states["postgres"] = "down: " + err.Error()
			ok = false
		} else {
			states["postgres"] = "up"
		}

		// Redis (optional dependency).
		if rc != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			err := rc.Ping(ctx).Err()
			cancel()
			if err != nil {
				states["redis"] = "down: " + err.Error()
				ok = false
			} else {
				states["redis"] = "up"
			}
		} else {
			states["redis"] = "skipped"
		}

		// Qdrant
		if qdrant != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
			err := qdrant.Ping(ctx)
			cancel()
			if err != nil {
				states["qdrant"] = "down: " + err.Error()
				ok = false
			} else {
				states["qdrant"] = "up"
			}
		} else {
			states["qdrant"] = "skipped"
		}

		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"ok": ok, "checks": states})
	}
}

func pingPostgres(ctx context.Context, db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx)
}
