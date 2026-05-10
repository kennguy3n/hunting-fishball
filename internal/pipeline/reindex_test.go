package pipeline_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeReindexEnum struct {
	docs map[string][]string // key = tenant|source|namespace
	err  error
}

func (f *fakeReindexEnum) ListDocuments(_ context.Context, req pipeline.ReindexRequest) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.docs[req.TenantID+"|"+req.SourceID+"|"+req.NamespaceID], nil
}

type reindexCaptureEmitter struct {
	events []pipeline.IngestEvent
	err    error
}

func (e *reindexCaptureEmitter) EmitEvent(_ context.Context, evt pipeline.IngestEvent) error {
	if e.err != nil {
		return e.err
	}
	e.events = append(e.events, evt)
	return nil
}

func TestReindexOrchestrator_Validation(t *testing.T) {
	t.Parallel()
	if _, err := pipeline.NewReindexOrchestrator(nil, &reindexCaptureEmitter{}); err == nil {
		t.Fatalf("expected error for nil enum")
	}
	if _, err := pipeline.NewReindexOrchestrator(&fakeReindexEnum{}, nil); err == nil {
		t.Fatalf("expected error for nil emitter")
	}
	r, _ := pipeline.NewReindexOrchestrator(&fakeReindexEnum{}, &reindexCaptureEmitter{})
	if _, err := r.Reindex(context.Background(), pipeline.ReindexRequest{}); err == nil {
		t.Fatalf("expected error for empty request")
	}
	if _, err := r.Reindex(context.Background(), pipeline.ReindexRequest{TenantID: "t"}); err == nil {
		t.Fatalf("expected error for missing source_id")
	}
}

func TestReindexOrchestrator_EmitsOneEventPerDocument(t *testing.T) {
	t.Parallel()
	enum := &fakeReindexEnum{docs: map[string][]string{
		"tenant-a|src-1|": {"doc-1", "doc-2", "doc-3"},
	}}
	em := &reindexCaptureEmitter{}
	r, _ := pipeline.NewReindexOrchestrator(enum, em)
	res, err := r.Reindex(context.Background(), pipeline.ReindexRequest{TenantID: "tenant-a", SourceID: "src-1"})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if res.EventsEmitted != 3 {
		t.Fatalf("emitted=%d", res.EventsEmitted)
	}
	if len(em.events) != 3 {
		t.Fatalf("captured=%d", len(em.events))
	}
	for _, evt := range em.events {
		if evt.Kind != pipeline.EventReindex {
			t.Fatalf("kind=%q", evt.Kind)
		}
		if evt.TenantID != "tenant-a" || evt.SourceID != "src-1" {
			t.Fatalf("scope mismatch: %+v", evt)
		}
	}
}

func TestReindexOrchestrator_NamespaceFilter(t *testing.T) {
	t.Parallel()
	enum := &fakeReindexEnum{docs: map[string][]string{
		"tenant-a|src-1|ns-a": {"doc-1"},
		"tenant-a|src-1|":     {"doc-A", "doc-B"},
	}}
	em := &reindexCaptureEmitter{}
	r, _ := pipeline.NewReindexOrchestrator(enum, em)
	res, _ := r.Reindex(context.Background(), pipeline.ReindexRequest{TenantID: "tenant-a", SourceID: "src-1", NamespaceID: "ns-a"})
	if res.EventsEmitted != 1 {
		t.Fatalf("expected 1 emission with ns filter, got %d", res.EventsEmitted)
	}
	if em.events[0].DocumentID != "doc-1" || em.events[0].NamespaceID != "ns-a" {
		t.Fatalf("event scope mismatch: %+v", em.events[0])
	}
}

func TestReindexOrchestrator_EnumeratorErrorIsFatal(t *testing.T) {
	t.Parallel()
	enum := &fakeReindexEnum{err: errors.New("db down")}
	em := &reindexCaptureEmitter{}
	r, _ := pipeline.NewReindexOrchestrator(enum, em)
	if _, err := r.Reindex(context.Background(), pipeline.ReindexRequest{TenantID: "t", SourceID: "s"}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestReindexOrchestrator_EmitErrorsAreCountedNotFatal(t *testing.T) {
	t.Parallel()
	enum := &fakeReindexEnum{docs: map[string][]string{
		"tenant-a|src-1|": {"doc-1", "doc-2"},
	}}
	em := &reindexCaptureEmitter{err: errors.New("kafka down")}
	r, _ := pipeline.NewReindexOrchestrator(enum, em)
	res, err := r.Reindex(context.Background(), pipeline.ReindexRequest{TenantID: "tenant-a", SourceID: "src-1"})
	if err != nil {
		t.Fatalf("reindex returned error: %v", err)
	}
	if res.EmitErrors != 2 || res.EventsEmitted != 0 {
		t.Fatalf("expected 2 emit errors, 0 events; got %+v", res)
	}
}

func TestReindexOrchestrator_ContextCancellation(t *testing.T) {
	t.Parallel()
	enum := &fakeReindexEnum{docs: map[string][]string{
		"tenant-a|src-1|": {"doc-1", "doc-2", "doc-3"},
	}}
	em := &reindexCaptureEmitter{}
	r, _ := pipeline.NewReindexOrchestrator(enum, em)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := r.Reindex(ctx, pipeline.ReindexRequest{TenantID: "tenant-a", SourceID: "src-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if res.EventsEmitted != 0 {
		t.Fatalf("expected 0 events under cancelled ctx, got %d", res.EventsEmitted)
	}
}
