// Package admin — index_health.go ships a per-backend health
// check endpoint.
//
// Round-4 Task 19: when a deploy goes sideways, the operator
// needs to know quickly whether each storage backend is
// reachable + has data. /v1/admin/health/indexes runs a Ping
// (or equivalent) against every storage adapter and returns a
// per-backend health record.
//
// The endpoint is read-only and does not depend on a tenant
// context — operators run it post-deploy and on-call. Only
// authentication is required (the authed group already
// enforces that).
package admin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// BackendStatus is one row in the health response.
type BackendStatus struct {
	Name      string        `json:"name"`
	Healthy   bool          `json:"healthy"`
	LatencyMS int64         `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	CheckedAt time.Time     `json:"checked_at"`
	Latency   time.Duration `json:"-"`
}

// IndexHealthResponse is the GET response.
type IndexHealthResponse struct {
	Healthy  bool            `json:"healthy"`
	Backends []BackendStatus `json:"backends"`
}

// BackendChecker is the contract every storage adapter implements
// for the index health check. The Name() returned identifies the
// backend in the response. Implementations MUST return promptly —
// the handler caps the per-check timeout via context.
type BackendChecker interface {
	Name() string
	Check(ctx context.Context) error
}

// IndexHealthHandler runs the registered checkers in parallel
// and returns the aggregate response.
type IndexHealthHandler struct {
	checkers []BackendChecker

	// Timeout caps every checker individually. Defaults to 5s.
	Timeout time.Duration
}

// NewIndexHealthHandler builds the handler with a list of
// checkers. Order is preserved in the response so operators
// always see backends in the same slot on the dashboard.
func NewIndexHealthHandler(checkers ...BackendChecker) *IndexHealthHandler {
	return &IndexHealthHandler{checkers: checkers}
}

// Register mounts the endpoint.
func (h *IndexHealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/health/indexes", h.health)
}

func (h *IndexHealthHandler) health(c *gin.Context) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	results := make([]BackendStatus, len(h.checkers))
	var wg sync.WaitGroup
	for i, ck := range h.checkers {
		wg.Add(1)
		go func(i int, ck BackendChecker) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
			defer cancel()
			start := time.Now()
			err := ck.Check(ctx)
			elapsed := time.Since(start)
			st := BackendStatus{
				Name:      ck.Name(),
				Healthy:   err == nil,
				CheckedAt: time.Now().UTC(),
				LatencyMS: elapsed.Milliseconds(),
				Latency:   elapsed,
			}
			if err != nil {
				st.Error = err.Error()
			}
			results[i] = st
		}(i, ck)
	}
	wg.Wait()

	allHealthy := true
	for _, r := range results {
		if !r.Healthy {
			allHealthy = false
			break
		}
	}
	resp := IndexHealthResponse{Healthy: allHealthy, Backends: results}
	if allHealthy {
		c.JSON(http.StatusOK, resp)
	} else {
		// 503 when ANY backend is unhealthy so a load-balancer
		// health probe pinned at this endpoint correctly
		// detaches the instance.
		c.JSON(http.StatusServiceUnavailable, resp)
	}
}

// PingChecker is a tiny wrapper that turns any (name, ping func)
// pair into a BackendChecker. Lets cmd/api wire the storage
// clients without each one implementing the interface itself.
type PingChecker struct {
	BackendName string
	PingFn      func(ctx context.Context) error
}

// Name returns the backend identifier.
func (p PingChecker) Name() string { return p.BackendName }

// Check delegates to the supplied ping function.
func (p PingChecker) Check(ctx context.Context) error {
	if p.PingFn == nil {
		return nil
	}
	return p.PingFn(ctx)
}
