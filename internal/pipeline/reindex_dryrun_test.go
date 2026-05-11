package pipeline_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type dryRunStubEnum struct{ docs []string }

func (s *dryRunStubEnum) ListDocuments(_ context.Context, _ pipeline.ReindexRequest) ([]string, error) {
	return s.docs, nil
}

type dryRunStubEmitter struct{ count atomic.Int32 }

func (s *dryRunStubEmitter) EmitEvent(_ context.Context, _ pipeline.IngestEvent) error {
	s.count.Add(1)
	return nil
}

func TestReindexOrchestrator_DryRun_NoEmit(t *testing.T) {
	enum := &dryRunStubEnum{docs: []string{"d1", "d2", "d3"}}
	emitter := &dryRunStubEmitter{}
	r, err := pipeline.NewReindexOrchestrator(enum, emitter)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := r.Reindex(context.Background(), pipeline.ReindexRequest{
		TenantID: "t1", SourceID: "s1", DryRun: true,
	})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.DocumentsEnumerated != 3 {
		t.Fatalf("expected 3 enumerated; got %d", res.DocumentsEnumerated)
	}
	if res.EventsEmitted != 0 {
		t.Fatalf("dry-run must emit nothing; got %d", res.EventsEmitted)
	}
	if !res.DryRun {
		t.Fatalf("DryRun flag not echoed in result")
	}
	if emitter.count.Load() != 0 {
		t.Fatalf("emitter called in dry-run: %d", emitter.count.Load())
	}
}

func TestReindexOrchestrator_NonDryRun_Emits(t *testing.T) {
	enum := &dryRunStubEnum{docs: []string{"d1", "d2"}}
	emitter := &dryRunStubEmitter{}
	r, _ := pipeline.NewReindexOrchestrator(enum, emitter)
	res, err := r.Reindex(context.Background(), pipeline.ReindexRequest{
		TenantID: "t1", SourceID: "s1",
	})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.EventsEmitted != 2 {
		t.Fatalf("expected 2 emitted; got %d", res.EventsEmitted)
	}
	if res.DryRun {
		t.Fatalf("DryRun must be false on a normal run")
	}
}
