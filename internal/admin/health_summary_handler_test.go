package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func init() { gin.SetMode(gin.TestMode) }

func TestHealthSummary_AllHealthy(t *testing.T) {
	t.Parallel()
	h := admin.NewHealthSummaryHandler(
		admin.SummaryProbeFunc{ProbeName: "postgres", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusHealthy, nil
		}},
		admin.SummaryProbeFunc{ProbeName: "qdrant", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusHealthy, nil
		}},
	)
	r := gin.New()
	h.Register(r.Group("/"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/health/summary", nil)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp admin.HealthSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != admin.SummaryStatusHealthy {
		t.Fatalf("overall=%q want healthy", resp.Status)
	}
	if len(resp.Components) != 2 {
		t.Fatalf("components=%d want 2", len(resp.Components))
	}
	for _, c := range resp.Components {
		if c.Status != admin.SummaryStatusHealthy {
			t.Errorf("%s status=%q want healthy", c.Name, c.Status)
		}
	}
}

func TestHealthSummary_DegradedReturns200(t *testing.T) {
	t.Parallel()
	h := admin.NewHealthSummaryHandler(
		admin.SummaryProbeFunc{ProbeName: "postgres", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusHealthy, nil
		}},
		admin.SummaryProbeFunc{ProbeName: "grpc_embedding", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusDegraded, nil
		}},
	)
	r := gin.New()
	h.Register(r.Group("/"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/health/summary", nil)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp admin.HealthSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != admin.SummaryStatusDegraded {
		t.Fatalf("overall=%q want degraded", resp.Status)
	}
}

func TestHealthSummary_FailingReturns503(t *testing.T) {
	t.Parallel()
	h := admin.NewHealthSummaryHandler(
		admin.SummaryProbeFunc{ProbeName: "postgres", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusHealthy, nil
		}},
		admin.SummaryProbeFunc{ProbeName: "qdrant", Fn: func(_ context.Context) (admin.SummaryStatus, error) {
			return admin.SummaryStatusHealthy, errors.New("connection refused")
		}},
	)
	r := gin.New()
	h.Register(r.Group("/"))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/health/summary", nil)
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rr.Code)
	}
	var resp admin.HealthSummaryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != admin.SummaryStatusUnhealthy {
		t.Fatalf("overall=%q want unhealthy", resp.Status)
	}
	var found bool
	for _, c := range resp.Components {
		if c.Name == "qdrant" {
			found = true
			if c.Status != admin.SummaryStatusUnhealthy {
				t.Errorf("qdrant status=%q", c.Status)
			}
			if c.Error == "" {
				t.Error("expected error string on failing component")
			}
		}
	}
	if !found {
		t.Fatal("qdrant component not present in response")
	}
}

func TestHealthSummary_ProbesRunInParallel(t *testing.T) {
	t.Parallel()
	// Each probe takes 100ms. With sequential execution the total
	// wall time would be 4*100=400ms; with parallel execution it
	// stays within 100ms+overhead.
	var calls atomic.Int32
	mk := func(name string) admin.SummaryProbe {
		return admin.SummaryProbeFunc{ProbeName: name, Fn: func(ctx context.Context) (admin.SummaryStatus, error) {
			calls.Add(1)
			select {
			case <-time.After(100 * time.Millisecond):
				return admin.SummaryStatusHealthy, nil
			case <-ctx.Done():
				return admin.SummaryStatusUnhealthy, ctx.Err()
			}
		}}
	}
	h := admin.NewHealthSummaryHandler(mk("a"), mk("b"), mk("c"), mk("d"))
	start := time.Now()
	resp := h.Run(context.Background())
	elapsed := time.Since(start)
	if calls.Load() != 4 {
		t.Fatalf("calls=%d want 4", calls.Load())
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("probes ran sequentially: elapsed=%v", elapsed)
	}
	if resp.Status != admin.SummaryStatusHealthy {
		t.Fatalf("overall=%q want healthy", resp.Status)
	}
}

func TestHealthSummary_TimeoutEnforced(t *testing.T) {
	t.Parallel()
	slow := admin.SummaryProbeFunc{ProbeName: "slow", Fn: func(ctx context.Context) (admin.SummaryStatus, error) {
		select {
		case <-time.After(2 * time.Second):
			return admin.SummaryStatusHealthy, nil
		case <-ctx.Done():
			return admin.SummaryStatusUnhealthy, ctx.Err()
		}
	}}
	h := admin.NewHealthSummaryHandler(slow)
	h.Timeout = 50 * time.Millisecond
	start := time.Now()
	resp := h.Run(context.Background())
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("timeout not enforced: elapsed=%v", time.Since(start))
	}
	if resp.Status != admin.SummaryStatusUnhealthy {
		t.Fatalf("overall=%q want unhealthy (timed-out probe)", resp.Status)
	}
}
