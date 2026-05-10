// Package capacity_test exercises the pipeline coordinator under
// load. It is intentionally a Go test (not a separate harness binary)
// so `go test ./tests/capacity/...` and `make capacity-test` produce
// identical results.
//
// Defaults are conservative — a few seconds at 600 docs/min — to keep
// CI fast. Override via env to run a longer soak locally:
//
//	CAPACITY_DOCS_PER_MIN=12000 CAPACITY_DURATION=2m \
//	  go test ./tests/capacity/... -run TestCapacityBaseline -v
//
// The test wires fake fetch/parse/embed/store stages that record
// per-document latency, then asserts every submit completed within
// the configured deadline (i.e. no back-pressure to the producer).
package capacity_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeFetch struct{}

func (fakeFetch) FetchEvent(_ context.Context, evt pipeline.IngestEvent) (*pipeline.Document, error) {
	return &pipeline.Document{
		TenantID:   evt.TenantID,
		SourceID:   evt.SourceID,
		DocumentID: evt.DocumentID,
		Content:    []byte("body"),
	}, nil
}

type fakeParse struct{}

func (fakeParse) Parse(_ context.Context, _ *pipeline.Document) ([]pipeline.Block, error) {
	return []pipeline.Block{{BlockID: "b1", Text: "x"}}, nil
}

type fakeEmbed struct{}

func (fakeEmbed) EmbedBlocks(_ context.Context, _ string, blocks []pipeline.Block) ([][]float32, string, error) {
	out := make([][]float32, len(blocks))
	for i := range out {
		out[i] = []float32{0, 0, 0}
	}
	return out, "fake-embed", nil
}

type fakeStore struct {
	mu       sync.Mutex
	finished int
}

func (s *fakeStore) Store(_ context.Context, _ *pipeline.Document, _ []pipeline.Block, _ [][]float32, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return nil
}

func (s *fakeStore) Delete(_ context.Context, _, _ string) error { return nil }

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func TestCapacityBaseline(t *testing.T) {
	docsPerMin, err := strconv.Atoi(envOrDefault("CAPACITY_DOCS_PER_MIN", "600"))
	if err != nil || docsPerMin <= 0 {
		t.Fatalf("CAPACITY_DOCS_PER_MIN: %v", err)
	}
	dur, err := time.ParseDuration(envOrDefault("CAPACITY_DURATION", "2s"))
	if err != nil || dur <= 0 {
		t.Fatalf("CAPACITY_DURATION: %v", err)
	}

	store := &fakeStore{}
	coord, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch:     fakeFetch{},
		Parse:     fakeParse{},
		Embed:     fakeEmbed{},
		Store:     store,
		QueueSize: 256,
		Workers: pipeline.StageConfig{
			FetchWorkers: 4,
			ParseWorkers: 4,
			EmbedWorkers: 4,
			StoreWorkers: 4,
		},
		MaxAttempts:    1,
		InitialBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coord: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dur+5*time.Second)
	defer cancel()
	doneCh := make(chan error, 1)
	go func() { doneCh <- coord.Run(ctx) }()

	interval := time.Minute / time.Duration(docsPerMin)
	deadline := time.Now().Add(dur)
	submitDeadline := 100 * time.Millisecond
	var submits int
	var maxSubmitLatency time.Duration
	latencies := make([]time.Duration, 0, docsPerMin)
	for time.Now().Before(deadline) {
		ts := time.Now()
		evt := pipeline.IngestEvent{
			TenantID:   "t",
			SourceID:   "s",
			DocumentID: fmt.Sprintf("d-%d", submits),
			Kind:       pipeline.EventDocumentChanged,
		}
		subCtx, subCancel := context.WithTimeout(ctx, submitDeadline)
		err := coord.Submit(subCtx, evt)
		subCancel()
		if err != nil {
			t.Fatalf("submit %d: %v", submits, err)
		}
		lat := time.Since(ts)
		if lat > maxSubmitLatency {
			maxSubmitLatency = lat
		}
		latencies = append(latencies, lat)
		submits++
		time.Sleep(interval)
	}
	coord.CloseInputs()
	if rerr := <-doneCh; rerr != nil {
		t.Fatalf("run: %v", rerr)
	}
	if maxSubmitLatency >= submitDeadline {
		t.Fatalf("submit back-pressure: max submit latency %v >= deadline %v", maxSubmitLatency, submitDeadline)
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[len(latencies)/2]
	p95 := latencies[(len(latencies)*95)/100]
	p99 := latencies[(len(latencies)*99)/100]
	t.Logf("submitted=%d completed=%d p50=%v p95=%v p99=%v", submits, store.finished, p50, p95, p99)
}
