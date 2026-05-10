package pipeline_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

type recorderAudit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (r *recorderAudit) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, log)
	return nil
}

// TestRetentionWorker_EmitsChunkExpired pins the contract that
// every successfully-deleted chunk produces exactly one
// chunk.expired audit row stamped with the chunk's metadata.
func TestRetentionWorker_EmitsChunkExpired(t *testing.T) {
	t.Parallel()
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
	rec := &recorderAudit{}
	w, err := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: &memDeleter{},
		Logger: discardLog(),
		Audit:  rec,
		Actor:  "00000000000000000000000000",
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := len(rec.logs); got != 2 {
		t.Fatalf("expected 2 chunk.expired events, got %d", got)
	}
	for _, l := range rec.logs {
		if l.Action != audit.ActionChunkExpired {
			t.Fatalf("action = %q, want chunk.expired", l.Action)
		}
		if l.TenantID != "tenant-a" {
			t.Fatalf("tenant_id = %q", l.TenantID)
		}
		if l.ResourceType != "chunk" {
			t.Fatalf("resource_type = %q, want chunk", l.ResourceType)
		}
	}
}

// TestRetentionWorker_NilAuditNeverEmits guards the default-off
// behaviour: existing deployments that don't pass an Audit field
// must not start writing rows after this commit.
func TestRetentionWorker_NilAuditNeverEmits(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunks := &memChunkSource{
		tenants: []string{"tenant-a"},
		byTen: map[string][]pipeline.ChunkRecord{
			"tenant-a": {{ID: "c1", TenantID: "tenant-a", SourceID: "s1", DocumentID: "d1", IngestedAt: now.Add(-30 * 24 * time.Hour)}},
		},
	}
	policies := &memPolicySource{byTen: map[string][]policy.RetentionPolicy{
		"tenant-a": {{TenantID: "tenant-a", Scope: policy.RetentionScopeTenant, MaxAgeDays: 7}},
	}}
	w, _ := pipeline.NewRetentionWorker(pipeline.RetentionWorkerConfig{
		Chunks: chunks, Policies: policies, Deleter: &memDeleter{},
		Logger: discardLog(),
		Now:    func() time.Time { return now },
	})
	if _, err := w.SweepOnce(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}
