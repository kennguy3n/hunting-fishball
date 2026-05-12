package observability_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

type fakePGStats struct{ open int }

func (f *fakePGStats) OpenConnections() int { return f.open }

func TestPoolLeakDetector_FiresOnSustainedSaturation(t *testing.T) {
	t.Parallel()
	src := &fakePGStats{open: 95}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := observability.NewPoolLeakDetector(observability.PoolLeakDetectorConfig{
		Pool:                   src,
		MaxOpen:                100,
		HighThresholdPercent:   90,
		HighConsecutiveSamples: 3,
		Logger:                 logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d.SampleOnce()
	}
	if !bytes.Contains(buf.Bytes(), []byte("postgres: pool saturation")) {
		t.Fatalf("expected saturation warning; got: %s", buf.String())
	}
	if d.LastPercent() != 95 {
		t.Fatalf("last percent=%d want 95", d.LastPercent())
	}
}

func TestPoolLeakDetector_DoesNotFireOnTransientSpike(t *testing.T) {
	t.Parallel()
	src := &fakePGStats{open: 95}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := observability.NewPoolLeakDetector(observability.PoolLeakDetectorConfig{
		Pool:                   src,
		MaxOpen:                100,
		HighThresholdPercent:   90,
		HighConsecutiveSamples: 3,
		Logger:                 logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.SampleOnce()
	d.SampleOnce()
	src.open = 50
	d.SampleOnce()
	if bytes.Contains(buf.Bytes(), []byte("postgres: pool saturation")) {
		t.Fatalf("expected no warning on transient spike; got: %s", buf.String())
	}
}

func TestPoolLeakDetector_RequiresMaxOpen(t *testing.T) {
	t.Parallel()
	_, err := observability.NewPoolLeakDetector(observability.PoolLeakDetectorConfig{
		Pool:    &fakePGStats{},
		MaxOpen: 0,
	})
	if err == nil {
		t.Fatal("expected error when MaxOpen <= 0")
	}
}
