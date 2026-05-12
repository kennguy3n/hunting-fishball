// Round-13 Task 1: structured health-check aggregator endpoint.
//
// GET /v1/admin/health/summary runs every registered health probe
// in parallel and returns one envelope with per-component status,
// latency, and an overall verdict (`healthy` / `degraded` /
// `unhealthy`). This complements GET /v1/admin/health/indexes
// (which only covers the storage backends) by also surfacing the
// gRPC circuit-breaker state for each sidecar and any other
// component the deployment registers.
//
// The verdict is computed from the per-probe results:
//
//   - all components healthy → overall = "healthy", HTTP 200
//   - one or more degraded   → overall = "degraded", HTTP 200
//   - one or more unhealthy  → overall = "unhealthy", HTTP 503
//
// "degraded" is reserved for probes that opt in to a three-valued
// result — for example, a gRPC pool whose circuit breaker is in
// the half-open state. Probes that only return error / nil collapse
// to healthy / unhealthy.
package admin

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// SummaryStatus is the three-valued health bucket used by both
// the per-component rows and the overall verdict.
type SummaryStatus string

const (
	// SummaryStatusHealthy means the probe succeeded outright.
	SummaryStatusHealthy SummaryStatus = "healthy"
	// SummaryStatusDegraded means the probe succeeded but the
	// component is in a non-nominal state (e.g. circuit breaker
	// half-open, cache miss-rate elevated).
	SummaryStatusDegraded SummaryStatus = "degraded"
	// SummaryStatusUnhealthy means the probe failed. The
	// aggregate verdict trips to unhealthy when any component
	// reports this status.
	SummaryStatusUnhealthy SummaryStatus = "unhealthy"
)

// SummaryProbe is the contract every component implements for
// the aggregator. Implementations MUST honour the context
// deadline and return promptly — the handler caps each probe at
// the configured Timeout.
type SummaryProbe interface {
	// Name identifies the component in the response. Use a
	// short snake_case label, e.g. "postgres", "qdrant",
	// "grpc_breaker_embedding".
	Name() string
	// Check returns the current status + optional context.
	// Returning an error implies SummaryStatusUnhealthy; the
	// Status field is honoured only when the error is nil.
	Check(ctx context.Context) (SummaryStatus, error)
}

// SummaryProbeFunc adapts a plain closure into a SummaryProbe.
type SummaryProbeFunc struct {
	ProbeName string
	Fn        func(ctx context.Context) (SummaryStatus, error)
}

// Name returns the configured probe name.
func (p SummaryProbeFunc) Name() string { return p.ProbeName }

// Check delegates to the closure. Nil closures return healthy so
// a misconfigured wiring fails closed (the closure being nil is
// almost always a programming mistake, not a real outage).
func (p SummaryProbeFunc) Check(ctx context.Context) (SummaryStatus, error) {
	if p.Fn == nil {
		return SummaryStatusHealthy, nil
	}
	return p.Fn(ctx)
}

// SummaryComponent is one row in the response envelope.
type SummaryComponent struct {
	Name      string        `json:"name"`
	Status    SummaryStatus `json:"status"`
	LatencyMS int64         `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	// Message is the operator-actionable note returned by
	// probes that opt in to the MessageProbe interface
	// (Round-19 Task 27). Usually paired with
	// SummaryStatusDegraded.
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// MessageProbe is the optional extension of SummaryProbe that
// returns an operator-actionable message alongside the status.
// The aggregator queries LastMessage after Check. Probes that
// don't implement this interface leave SummaryComponent.Message
// blank. Round-19 Task 27.
type MessageProbe interface {
	SummaryProbe
	LastMessage() string
}

// HealthSummaryResponse is the JSON shape returned by
// GET /v1/admin/health/summary.
type HealthSummaryResponse struct {
	Status     SummaryStatus      `json:"status"`
	Components []SummaryComponent `json:"components"`
	CheckedAt  time.Time          `json:"checked_at"`
	DurationMS int64              `json:"duration_ms"`
}

// HealthSummaryHandler aggregates per-component probes.
type HealthSummaryHandler struct {
	probes []SummaryProbe
	// Timeout caps every probe individually. Defaults to 3s.
	Timeout time.Duration
	// nowFn is overridable in tests so the timestamps are
	// deterministic.
	nowFn func() time.Time
}

// NewHealthSummaryHandler wires the handler with a fixed
// component list. Order is preserved in the response so the
// admin dashboard renders the same slot for each component.
func NewHealthSummaryHandler(probes ...SummaryProbe) *HealthSummaryHandler {
	return &HealthSummaryHandler{probes: probes}
}

// Register mounts GET /v1/admin/health/summary on rg.
func (h *HealthSummaryHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/health/summary", h.summary)
}

func (h *HealthSummaryHandler) now() time.Time {
	if h.nowFn != nil {
		return h.nowFn()
	}
	return time.Now().UTC()
}

func (h *HealthSummaryHandler) summary(c *gin.Context) {
	resp := h.Run(c.Request.Context())
	status := http.StatusOK
	if resp.Status == SummaryStatusUnhealthy {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, resp)
}

// Run is the testable entry point: it fans out the registered
// probes, aggregates the verdict, and returns the response. The
// HTTP handler is a thin wrapper around this method.
func (h *HealthSummaryHandler) Run(ctx context.Context) HealthSummaryResponse {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	results := make([]SummaryComponent, len(h.probes))
	var wg sync.WaitGroup
	overallStart := time.Now()
	for i, p := range h.probes {
		wg.Add(1)
		go func(i int, p SummaryProbe) {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			start := time.Now()
			status, err := p.Check(probeCtx)
			elapsed := time.Since(start)
			row := SummaryComponent{
				Name:      p.Name(),
				LatencyMS: elapsed.Milliseconds(),
				CheckedAt: h.now(),
			}
			if err != nil {
				row.Status = SummaryStatusUnhealthy
				row.Error = err.Error()
			} else {
				switch status {
				case SummaryStatusDegraded:
					row.Status = SummaryStatusDegraded
				case SummaryStatusUnhealthy:
					row.Status = SummaryStatusUnhealthy
				default:
					row.Status = SummaryStatusHealthy
				}
			}
			// Round-19 Task 27 — surface an operator-actionable
			// message from probes that opt in.
			if mp, ok := p.(MessageProbe); ok {
				row.Message = mp.LastMessage()
			}
			results[i] = row
		}(i, p)
	}
	wg.Wait()
	// Stable component ordering for consumers that hash the
	// response body (e.g. dashboard renders).
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	verdict := SummaryStatusHealthy
	for _, r := range results {
		switch r.Status {
		case SummaryStatusUnhealthy:
			verdict = SummaryStatusUnhealthy
		case SummaryStatusDegraded:
			if verdict != SummaryStatusUnhealthy {
				verdict = SummaryStatusDegraded
			}
		}
	}
	return HealthSummaryResponse{
		Status:     verdict,
		Components: results,
		CheckedAt:  h.now(),
		DurationMS: time.Since(overallStart).Milliseconds(),
	}
}
