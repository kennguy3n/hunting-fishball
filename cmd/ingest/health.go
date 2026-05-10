// health.go — Phase 8 Task 20: HTTP probes for cmd/ingest plus the
// /metrics endpoint scraped by Prometheus. The ingest binary
// historically had no HTTP surface; we mount a sidecar listener so
// k8s probes + the HPA can both target the same pod port.
package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// ingestHTTPHandler builds the mux for the ingest probe / metrics
// endpoints. /healthz is liveness only; /readyz checks Postgres,
// Redis (optional), and TCP-connectivity to every Kafka broker.
func ingestHTTPHandler(db *gorm.DB, rc *redis.Client, brokers []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		states := map[string]string{}
		ok := true

		// Postgres
		if err := pingPostgres(r.Context(), db); err != nil {
			states["postgres"] = "down: " + err.Error()
			ok = false
		} else {
			states["postgres"] = "up"
		}

		// Redis (optional).
		if rc != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
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

		// Kafka brokers — TCP-only check; sarama would need a fresh
		// client per probe which is too heavy for a probe path.
		states["kafka"] = "up"
		for _, addr := range brokers {
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				states["kafka"] = "down: " + addr + ": " + derr.Error()
				ok = false
				break
			}
			_ = c.Close()
		}

		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "checks": states})
	})
	mux.Handle("/metrics", observability.Handler())
	return mux
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
