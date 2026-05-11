// Round-12 Task 3: retention worker metrics. The Sweep loop must
// bump `context_engine_retention_expired_chunks_total` once per
// successfully-deleted chunk and observe the wall-clock duration
// into `context_engine_retention_sweep_duration_seconds`. The
// counter MUST NOT advance on a failed delete — operators only want
// to see chunks that actually left the storage tier.
package pipeline_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

func TestRetentionWorker_Metrics_ExpiredCounter(t *testing.T) {
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {
				{ID: "c1", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
				{ID: "c2", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
			},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 7}},
	}}
	w, err := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: &memDeleter{},
		Logger: discardLog(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	before := testutil.ToFloat64(observability.RetentionExpiredChunksTotal)
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := testutil.ToFloat64(observability.RetentionExpiredChunksTotal) - before
	if got != 2 {
		t.Fatalf("expected counter delta=2 after sweeping 2 expired chunks, got %v", got)
	}
}

func TestRetentionWorker_Metrics_CounterDoesNotAdvanceOnDeleteFailure(t *testing.T) {
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {
				{ID: "fail-me", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
				{ID: "ok", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
			},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 7}},
	}}
	d := &memDeleter{failOn: "fail-me"}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
		Now: func() time.Time { return now },
	})

	before := testutil.ToFloat64(observability.RetentionExpiredChunksTotal)
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := testutil.ToFloat64(observability.RetentionExpiredChunksTotal) - before
	if got != 1 {
		t.Fatalf("expected counter delta=1 (only successful delete counts), got %v", got)
	}
}
