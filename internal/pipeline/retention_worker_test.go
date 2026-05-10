package pipeline_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// memChunkSource is an in-memory RetentionChunkSource.
type memChunkSource struct {
	tenants []string
	byTen   map[string][]pipeline.ChunkRecord
}

func (s *memChunkSource) ListTenants(_ context.Context) ([]string, error) { return s.tenants, nil }
func (s *memChunkSource) ListChunks(_ context.Context, t string) ([]pipeline.ChunkRecord, error) {
	return s.byTen[t], nil
}

// memPolicySource is an in-memory RetentionPolicySource.
type memPolicySource struct {
	byTen map[string][]policy.RetentionPolicy
	err   error
}

func (s *memPolicySource) List(_ context.Context, t string) ([]policy.RetentionPolicy, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byTen[t], nil
}

// memDeleter records every delete the worker requested.
type memDeleter struct {
	mu      sync.Mutex
	deleted []string
	failOn  string
}

func (d *memDeleter) DeleteChunk(_ context.Context, _, _, chunkID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if chunkID == d.failOn {
		return errors.New("simulated delete failure")
	}
	d.deleted = append(d.deleted, chunkID)
	return nil
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRetentionWorker_NewRetentionWorker_Validation(t *testing.T) {
	t.Parallel()
	if _, err := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{}); err == nil {
		t.Fatalf("expected error for empty config")
	}
}

func TestRetentionWorker_DeletesExpiredChunks(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {
				{ID: "c1", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
				{ID: "c2", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-2 * 24 * time.Hour)},
				{ID: "c3", TenantID: "tenant-a", SourceID: "s2", DocumentID: "d3", IngestedAt: now.Add(-100 * 24 * time.Hour)},
			},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 7}},
	}}
	d := &memDeleter{}
	w, err := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := w.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.ChunksDeleted != 2 || res.ChunksExpired != 2 {
		t.Fatalf("unexpected sweep result: %+v", res)
	}
	if len(d.deleted) != 2 {
		t.Fatalf("expected 2 deletes, got %d", len(d.deleted))
	}
}

func TestRetentionWorker_NoPolicy_NoDeletes(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {{ID: "c1", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-365 * 24 * time.Hour)}},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{}}
	d := &memDeleter{}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
	})
	res, _ := w.SweepOnce(context.Background())
	if res.ChunksDeleted != 0 || res.ChunksExpired != 0 {
		t.Fatalf("no policy must produce no deletes; got %+v", res)
	}
}

func TestRetentionWorker_NamespaceLayerWinsWhenMoreRestrictive(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {
				{ID: "old-doc", TenantID: "tenant-a", SourceID: "s1", NamespaceID: "ns-a", DocumentID: "d1", IngestedAt: now.Add(-10 * 24 * time.Hour)},
			},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {
			{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 30},
			{TenantID: "tenant-a", Scope: policy.RetentionScopeNamespace, ScopeValue: "s1/ns-a", MaxAgeDays: 7},
		},
	}}
	d := &memDeleter{}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
		Now: func() time.Time { return now },
	})
	res, _ := w.SweepOnce(context.Background())
	if res.ChunksDeleted != 1 {
		t.Fatalf("expected namespace policy to evict; got %+v", res)
	}
}

func TestRetentionWorker_DeleteFailureContinues(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {
				{ID: "fail-me", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)},
				{ID: "next", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d2", IngestedAt: now.Add(-30 * 24 * time.Hour)},
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
	res, _ := w.SweepOnce(context.Background())
	if res.ChunksExpired != 2 || res.ChunksDeleted != 1 {
		t.Fatalf("expected 1 delete, 2 expired; got %+v", res)
	}
}

func TestRetentionWorker_TenantIsolation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a", "tenant-b"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {{ID: "c1", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)}},
			"tenant-b": {{ID: "c2", TenantID: "tenant-b", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)}},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 7}},
		"tenant-b": {},
	}}
	d := &memDeleter{}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
		Now: func() time.Time { return now },
	})
	_, _ = w.SweepOnce(context.Background())
	if len(d.deleted) != 1 || d.deleted[0] != "c1" {
		t.Fatalf("only tenant-a should evict, got %+v", d.deleted)
	}
}

func TestRetentionWorker_PolicyErrorIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	chunks := &memChunkSource{tenants: []string{"tenant-a"}, byTen: map[string][]pipeline.ChunkRecord{}}
	policies := &memPolicySource{err: errors.New("db down")}
	d := &memDeleter{}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: d, Logger: discardLog(),
	})
	if err := w.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep should swallow per-tenant errors: %v", err)
	}
}
