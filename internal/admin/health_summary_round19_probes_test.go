package admin_test

// health_summary_round19_probes_test.go — Round-19 Task 27.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

type fakeStaleReader struct {
	count int
	err   error
}

func (f *fakeStaleReader) CountStale(_ context.Context, _ time.Duration) (int, error) {
	return f.count, f.err
}

func TestStaleConnectorsProbe_Healthy(t *testing.T) {
	t.Parallel()
	p := &admin.StaleConnectorsProbe{Reader: &fakeStaleReader{count: 0}}
	status, err := p.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != admin.SummaryStatusHealthy {
		t.Fatalf("expected healthy, got %q", status)
	}
}

func TestStaleConnectorsProbe_DegradedAndUnhealthy(t *testing.T) {
	t.Parallel()
	p := &admin.StaleConnectorsProbe{Reader: &fakeStaleReader{count: 2}, UnhealthyAt: 5}
	status, err := p.Check(context.Background())
	if err != nil || status != admin.SummaryStatusDegraded {
		t.Fatalf("expected degraded, got status=%q err=%v", status, err)
	}
	if p.LastMessage() == "" {
		t.Fatal("expected non-empty operator message")
	}
	p2 := &admin.StaleConnectorsProbe{Reader: &fakeStaleReader{count: 7}, UnhealthyAt: 5}
	status2, _ := p2.Check(context.Background())
	if status2 != admin.SummaryStatusUnhealthy {
		t.Fatalf("expected unhealthy at >=5 stale, got %q", status2)
	}
}

type fakeDLQGrowthReader struct {
	growth int
	err    error
}

func (f *fakeDLQGrowthReader) GrowthOverWindow(_ context.Context, _ time.Duration) (int, error) {
	return f.growth, f.err
}

func TestDLQGrowthProbe_Tiers(t *testing.T) {
	t.Parallel()
	healthy := &admin.DLQGrowthProbe{Reader: &fakeDLQGrowthReader{growth: 5}, DegradeAt: 50, UnhealthyAt: 500}
	if s, _ := healthy.Check(context.Background()); s != admin.SummaryStatusHealthy {
		t.Fatalf("healthy: got %q", s)
	}
	deg := &admin.DLQGrowthProbe{Reader: &fakeDLQGrowthReader{growth: 100}, DegradeAt: 50, UnhealthyAt: 500}
	if s, _ := deg.Check(context.Background()); s != admin.SummaryStatusDegraded {
		t.Fatalf("degraded: got %q", s)
	}
	un := &admin.DLQGrowthProbe{Reader: &fakeDLQGrowthReader{growth: 600}, DegradeAt: 50, UnhealthyAt: 500}
	if s, _ := un.Check(context.Background()); s != admin.SummaryStatusUnhealthy {
		t.Fatalf("unhealthy: got %q", s)
	}
}

type fakeEmbeddingPinger struct{ err error }

func (f *fakeEmbeddingPinger) PingEmbeddingModel(_ context.Context) error { return f.err }

func TestEmbeddingModelProbe(t *testing.T) {
	t.Parallel()
	p := &admin.EmbeddingModelProbe{Pinger: &fakeEmbeddingPinger{}}
	if s, _ := p.Check(context.Background()); s != admin.SummaryStatusHealthy {
		t.Fatalf("healthy: %q", s)
	}
	p2 := &admin.EmbeddingModelProbe{Pinger: &fakeEmbeddingPinger{err: errors.New("rpc down")}}
	s, err := p2.Check(context.Background())
	if err == nil || s != admin.SummaryStatusUnhealthy {
		t.Fatalf("unhealthy: s=%q err=%v", s, err)
	}
	if p2.LastMessage() == "" {
		t.Fatal("expected non-empty operator message on unhealthy")
	}
}

type fakeTantivyReader struct {
	used  int64
	total int64
	err   error
}

func (f *fakeTantivyReader) IndexBytesUsed(_ context.Context) (int64, int64, error) {
	return f.used, f.total, f.err
}

func TestTantivyDiskProbe_Tiers(t *testing.T) {
	t.Parallel()
	healthy := &admin.TantivyDiskProbe{Reader: &fakeTantivyReader{used: 100, total: 1000}, Warn: 0.7, Critical: 0.9}
	if s, _ := healthy.Check(context.Background()); s != admin.SummaryStatusHealthy {
		t.Fatalf("healthy: %q", s)
	}
	warn := &admin.TantivyDiskProbe{Reader: &fakeTantivyReader{used: 800, total: 1000}, Warn: 0.7, Critical: 0.9}
	if s, _ := warn.Check(context.Background()); s != admin.SummaryStatusDegraded {
		t.Fatalf("degraded: %q", s)
	}
	crit := &admin.TantivyDiskProbe{Reader: &fakeTantivyReader{used: 950, total: 1000}, Warn: 0.7, Critical: 0.9}
	if s, _ := crit.Check(context.Background()); s != admin.SummaryStatusUnhealthy {
		t.Fatalf("unhealthy: %q", s)
	}
}
