// Package benchmark holds Phase 3 acceptance benchmarks. The
// benchmarks measure end-to-end ingest pipeline throughput and
// retrieval fan-out latency against the same in-process fakes the
// unit tests use, so they can run on any laptop without docker.
//
// Headline numbers (docs/sec, P50/P95/P99 retrieval latency, RSS
// per worker) are emitted via b.ReportMetric so `go test -bench .
// -benchmem` produces a single line per metric. The cutover doc
// quotes these numbers as Phase 3 acceptance criteria.
//
// Run: `go test -bench . -benchmem -benchtime=3s ./tests/benchmark/`.
package benchmark

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// fakeFetch returns deterministic content from the inline byte slice.
type fakeFetch struct{}

func (fakeFetch) FetchEvent(_ context.Context, evt pipeline.IngestEvent) (*pipeline.Document, error) {
	return &pipeline.Document{
		TenantID:   evt.TenantID,
		SourceID:   evt.SourceID,
		DocumentID: evt.DocumentID,
		Content:    []byte("benchmark fetch content " + evt.DocumentID),
		MIMEType:   "text/plain",
	}, nil
}

// fakeParse splits the content into a fixed number of blocks.
type fakeParse struct {
	blocksPerDoc int
}

func (p fakeParse) Parse(_ context.Context, doc *pipeline.Document) ([]pipeline.Block, error) {
	n := p.blocksPerDoc
	if n == 0 {
		n = 5
	}
	out := make([]pipeline.Block, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pipeline.Block{
			BlockID:  doc.DocumentID + "-b" + iToS(i),
			Text:     "block text body " + doc.DocumentID,
			Position: int32(i),
		})
	}

	return out, nil
}

// fakeEmbed produces a deterministic 16-dim vector per block.
type fakeEmbed struct{}

func (fakeEmbed) EmbedBlocks(_ context.Context, _ string, blocks []pipeline.Block) ([][]float32, string, error) {
	out := make([][]float32, 0, len(blocks))
	for i := range blocks {
		v := make([]float32, 16)
		for j := range v {
			v[j] = float32((i+1)*(j+1)) / 100.0
		}
		out = append(out, v)
	}

	return out, "fake-embed", nil
}

// fakeStore drops the result on the floor — we're benchmarking the
// pipeline coordination cost, not the storage layer.
type fakeStore struct{}

func (fakeStore) Store(_ context.Context, _ *pipeline.Document, _ []pipeline.Block, _ [][]float32, _ string) error {
	return nil
}

func (fakeStore) Delete(_ context.Context, _, _ string) error {
	return nil
}

// runPipeline runs the coordinator with the supplied workload and
// returns the wall-clock elapsed plus the success count.
func runPipeline(b *testing.B, n int) (time.Duration, int) {
	b.Helper()

	var done atomic.Int64
	var wg sync.WaitGroup

	c, err := pipeline.NewCoordinator(pipeline.CoordinatorConfig{
		Fetch: fakeFetch{},
		Parse: fakeParse{blocksPerDoc: 5},
		Embed: fakeEmbed{},
		Store: fakeStore{},
		OnSuccess: func(_ context.Context, _ pipeline.IngestEvent) {
			done.Add(1)
		},
		OnDLQ: func(_ context.Context, _ pipeline.IngestEvent, _ error) {
			done.Add(1)
		},
	})
	if err != nil {
		b.Fatalf("NewCoordinator: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = c.Run(ctx)
	}()

	start := time.Now()
	for i := 0; i < n; i++ {
		evt := pipeline.IngestEvent{
			TenantID:   "bench-tenant",
			SourceID:   "bench-source",
			DocumentID: "doc-" + iToS(i),
			Kind:       pipeline.EventDocumentChanged,
		}
		if err := c.Submit(ctx, evt); err != nil {
			b.Fatalf("Submit: %v", err)
		}
	}
	// Wait for completion.
	for done.Load() < int64(n) {
		runtime.Gosched()
		time.Sleep(50 * time.Microsecond)
	}
	elapsed := time.Since(start)
	cancel()
	wg.Wait()

	return elapsed, int(done.Load())
}

// BenchmarkPipeline_Throughput measures sustained docs/sec through
// the ingest pipeline. We feed b.N events into the coordinator and
// report `docs/sec` and the heap usage so the cutover doc can quote
// real numbers.
func BenchmarkPipeline_Throughput(b *testing.B) {
	if b.N <= 0 {
		return
	}
	var startMem, endMem runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&startMem)

	b.ResetTimer()
	elapsed, _ := runPipeline(b, b.N)
	b.StopTimer()

	runtime.ReadMemStats(&endMem)

	docsPerSec := float64(b.N) / elapsed.Seconds()
	b.ReportMetric(docsPerSec, "docs/sec")
	b.ReportMetric(float64(endMem.HeapInuse-startMem.HeapInuse)/(1024*1024), "heap_MB")
}

func iToS(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := [20]byte{}
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}

	return string(buf[pos:])
}
