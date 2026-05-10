package pipeline

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type recordingAuditSink struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (r *recordingAuditSink) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}

func (r *recordingAuditSink) snapshot() []*audit.AuditLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*audit.AuditLog, len(r.logs))
	copy(out, r.logs)
	return out
}

func TestDeduplicator_Disabled_NoOp(t *testing.T) {
	d := NewDeduplicator(DedupConfig{Enabled: false})
	if d.Enabled() {
		t.Fatalf("Enabled() should be false")
	}
	res, err := d.Apply(context.Background(), &Document{TenantID: "t", DocumentID: "d"}, []Block{{BlockID: "1"}, {BlockID: "2"}}, [][]float32{{1, 0}, {1, 0}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Removed != 0 || len(res.Blocks) != 2 {
		t.Fatalf("disabled dedup must keep all blocks; got removed=%d kept=%d", res.Removed, len(res.Blocks))
	}
}

func TestDeduplicator_IdenticalDuplicate(t *testing.T) {
	sink := &recordingAuditSink{}
	d := NewDeduplicator(DedupConfig{Enabled: true, Threshold: 0.95, Audit: sink, Connector: "test"})
	doc := &Document{TenantID: "t", DocumentID: "d", SourceID: "s"}
	blocks := []Block{{BlockID: "1"}, {BlockID: "2"}}
	emb := [][]float32{{1, 0, 0}, {1, 0, 0}}
	res, err := d.Apply(context.Background(), doc, blocks, emb)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("expected one removal; got %d", res.Removed)
	}
	if len(res.Blocks) != 1 || res.Blocks[0].BlockID != "1" {
		t.Fatalf("expected first block to be kept; got %v", res.Blocks)
	}
	logs := sink.snapshot()
	if len(logs) != 1 || logs[0].Action != audit.ActionChunkDeduplicated {
		t.Fatalf("expected one chunk.deduplicated audit; got %v", logs)
	}
}

func TestDeduplicator_DissimilarKept(t *testing.T) {
	d := NewDeduplicator(DedupConfig{Enabled: true, Threshold: 0.99})
	doc := &Document{TenantID: "t", DocumentID: "d"}
	blocks := []Block{{BlockID: "1"}, {BlockID: "2"}}
	emb := [][]float32{{1, 0, 0}, {0, 1, 0}}
	res, err := d.Apply(context.Background(), doc, blocks, emb)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Removed != 0 {
		t.Fatalf("orthogonal vectors must not be deduped; got %d removals", res.Removed)
	}
}

func TestDeduplicator_ThresholdBoundary(t *testing.T) {
	cases := []struct {
		name      string
		threshold float32
		sim       float32 // simulated cosine; vec construction below
		removed   int
	}{
		{"just-below-threshold-keeps", 0.95, 0.94, 0},
		{"at-threshold-removes", 0.95, 0.95, 1},
		{"above-threshold-removes", 0.95, 0.99, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Construct two unit vectors whose cosine is `tc.sim`
			// in 2D: a=(1,0), b=(sim, sqrt(1-sim^2)).
			a := []float32{1, 0}
			s := float32(tc.sim)
			b := []float32{s, sqrt32(1 - s*s)}
			d := NewDeduplicator(DedupConfig{Enabled: true, Threshold: tc.threshold})
			res, err := d.Apply(context.Background(), &Document{TenantID: "t", DocumentID: "d"},
				[]Block{{BlockID: "1"}, {BlockID: "2"}},
				[][]float32{a, b})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if res.Removed != tc.removed {
				t.Fatalf("threshold=%v sim=%v want removed=%d got %d",
					tc.threshold, tc.sim, tc.removed, res.Removed)
			}
		})
	}
}

func TestLoadDedupConfigFromEnv(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_DEDUP_ENABLED", "true")
	t.Setenv("CONTEXT_ENGINE_DEDUP_THRESHOLD", "0.85")
	cfg := LoadDedupConfigFromEnv()
	if !cfg.Enabled || cfg.Threshold != 0.85 {
		t.Fatalf("env not honoured: %+v", cfg)
	}
	t.Setenv("CONTEXT_ENGINE_DEDUP_ENABLED", "")
	cfg = LoadDedupConfigFromEnv()
	if cfg.Enabled {
		t.Fatalf("missing env should disable")
	}
}

func TestCosineSimilarityBasics(t *testing.T) {
	if got := cosineSimilarity([]float32{1, 0}, []float32{1, 0}); got < 0.999 {
		t.Fatalf("identical = ~1; got %v", got)
	}
	if got := cosineSimilarity([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Fatalf("orthogonal = 0; got %v", got)
	}
	if got := cosineSimilarity(nil, []float32{1}); got != 0 {
		t.Fatalf("nil = 0; got %v", got)
	}
}

func sqrt32(x float32) float32 {
	if x <= 0 {
		return 0
	}
	// Newton's method, sufficient precision for our boundary
	// tests; avoids importing math from the test file.
	g := x / 2
	for i := 0; i < 12; i++ {
		g = 0.5 * (g + x/g)
	}
	return g
}

func TestMain(m *testing.M) {
	// Make sure CONTEXT_ENGINE_DEDUP_* don't leak into other
	// tests in this package (e.g. coordinator tests below).
	_ = os.Unsetenv("CONTEXT_ENGINE_DEDUP_ENABLED")
	_ = os.Unsetenv("CONTEXT_ENGINE_DEDUP_THRESHOLD")
	os.Exit(m.Run())
}
