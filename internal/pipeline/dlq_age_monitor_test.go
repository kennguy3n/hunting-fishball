package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// fakeOldestLister is a no-db DLQOldestLister fake whose behaviour
// is driven by the configured oldest, err pair.
type fakeOldestLister struct {
	oldest time.Time
	err    error
	calls  int
}

func (f *fakeOldestLister) OldestUnresolved(_ context.Context) (time.Time, error) {
	f.calls++
	return f.oldest, f.err
}

// gaugeValue scrapes the registered gauge value.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

func TestDLQAgeMonitor_PublishesAgeSeconds(t *testing.T) {
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	lister := &fakeOldestLister{oldest: old}
	mon, err := pipeline.NewDLQAgeMonitor(pipeline.DLQAgeMonitorConfig{
		Lister:   lister,
		Interval: 50 * time.Millisecond,
		NowFn:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewDLQAgeMonitor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = mon.Run(ctx)
	got := gaugeValue(t, observability.DLQOldestMessageAgeSeconds)
	want := 2 * 3600.0
	if got < want-1 || got > want+1 {
		t.Fatalf("age gauge=%.2fs want≈%.2fs", got, want)
	}
}

func TestDLQAgeMonitor_NoUnresolvedRowsZerosGauge(t *testing.T) {
	// First seed a non-zero value, then run with the empty case
	// and assert the monitor resets to 0.
	observability.DLQOldestMessageAgeSeconds.Set(9999)
	lister := &fakeOldestLister{err: pipeline.ErrNoUnresolvedRows}
	mon, err := pipeline.NewDLQAgeMonitor(pipeline.DLQAgeMonitorConfig{
		Lister:   lister,
		Interval: 50 * time.Millisecond,
		NowFn:    func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewDLQAgeMonitor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = mon.Run(ctx)
	got := gaugeValue(t, observability.DLQOldestMessageAgeSeconds)
	if got != 0 {
		t.Fatalf("gauge=%v want=0 after empty DLQ", got)
	}
}

func TestDLQAgeMonitor_DBErrorDoesNotFlap(t *testing.T) {
	observability.DLQOldestMessageAgeSeconds.Set(120)
	lister := &fakeOldestLister{err: errors.New("boom")}
	mon, err := pipeline.NewDLQAgeMonitor(pipeline.DLQAgeMonitorConfig{
		Lister:   lister,
		Interval: 50 * time.Millisecond,
		NowFn:    func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("NewDLQAgeMonitor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	_ = mon.Run(ctx)
	got := gaugeValue(t, observability.DLQOldestMessageAgeSeconds)
	if got != 120 {
		t.Fatalf("gauge mutated on transient error: got=%v want=120", got)
	}
}

func TestDLQAgeMonitor_RequiresLister(t *testing.T) {
	t.Parallel()
	_, err := pipeline.NewDLQAgeMonitor(pipeline.DLQAgeMonitorConfig{})
	if err == nil || !strings.Contains(err.Error(), "Lister") {
		t.Fatalf("err=%v", err)
	}
}
