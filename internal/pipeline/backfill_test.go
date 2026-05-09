package pipeline_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// captureEmitter implements pipeline.BackfillEmitter, capturing
// every event for assertions.
type captureEmitter struct {
	mu     sync.Mutex
	events []pipeline.IngestEvent
	err    error
}

func (c *captureEmitter) EmitEvent(_ context.Context, evt pipeline.IngestEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.events = append(c.events, evt)
	return nil
}

func (c *captureEmitter) snapshot() []pipeline.IngestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]pipeline.IngestEvent, len(c.events))
	copy(out, c.events)
	return out
}

// fakeIterator yields a fixed list of DocumentRefs.
type fakeIterator struct {
	refs []connector.DocumentRef
	idx  int
}

func (f *fakeIterator) Next(_ context.Context) bool {
	if f.idx >= len(f.refs) {
		return false
	}
	f.idx++
	return true
}

func (f *fakeIterator) Doc() connector.DocumentRef { return f.refs[f.idx-1] }
func (f *fakeIterator) Err() error                 { return nil }
func (f *fakeIterator) Close() error               { return nil }

// fakeConnector implements connector.SourceConnector with
// configurable ListNamespaces / ListDocuments behaviour. Only the
// methods backfill cares about are wired; the rest panic.
type fakeConnector struct {
	namespaces []connector.Namespace
	docs       map[string][]connector.DocumentRef // namespaceID → refs
	listErr    error
}

func (f *fakeConnector) Validate(_ context.Context, _ connector.ConnectorConfig) error {
	return nil
}
func (f *fakeConnector) Connect(_ context.Context, _ connector.ConnectorConfig) (connector.Connection, error) {
	return nil, nil
}
func (f *fakeConnector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.namespaces, nil
}
func (f *fakeConnector) ListDocuments(_ context.Context, _ connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	return &fakeIterator{refs: f.docs[ns.ID]}, nil
}
func (f *fakeConnector) FetchDocument(_ context.Context, _ connector.Connection, _ connector.DocumentRef) (*connector.Document, error) {
	panic("not used in backfill test")
}
func (f *fakeConnector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	panic("not used in backfill test")
}
func (f *fakeConnector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// fakeConn is a minimal connector.Connection for tests.
type fakeConn struct{ tenant, source string }

func (f fakeConn) TenantID() string { return f.tenant }
func (f fakeConn) SourceID() string { return f.source }

func TestBackfill_AllNamespacesEmitBackfillEvents(t *testing.T) {
	t.Parallel()
	cap := &captureEmitter{}
	bf, err := pipeline.NewBackfill(pipeline.BackfillConfig{Producer: cap})
	if err != nil {
		t.Fatalf("NewBackfill: %v", err)
	}
	conn := &fakeConnector{
		namespaces: []connector.Namespace{{ID: "drive-1"}, {ID: "drive-2"}},
		docs: map[string][]connector.DocumentRef{
			"drive-1": {{ID: "d1"}, {ID: "d2"}},
			"drive-2": {{ID: "d3"}},
		},
	}
	res, err := bf.Run(context.Background(), pipeline.BackfillRequest{
		TenantID: "tenant-a", SourceID: "src-1",
		Connector: conn, Conn: fakeConn{tenant: "tenant-a", source: "src-1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Namespaces != 2 || res.Documents != 3 {
		t.Fatalf("result: %+v", res)
	}
	for _, e := range cap.snapshot() {
		if e.SyncMode != pipeline.SyncModeBackfill {
			t.Fatalf("expected SyncModeBackfill, got %q (event=%+v)", e.SyncMode, e)
		}
		if e.TenantID != "tenant-a" || e.SourceID != "src-1" {
			t.Fatalf("tenant/source mismatch: %+v", e)
		}
	}
}

func TestBackfill_ScopesFilterNamespaces(t *testing.T) {
	t.Parallel()
	cap := &captureEmitter{}
	bf, _ := pipeline.NewBackfill(pipeline.BackfillConfig{Producer: cap})
	conn := &fakeConnector{
		namespaces: []connector.Namespace{
			{ID: "drive-1"}, {ID: "drive-2"}, {ID: "shared-1"},
		},
		docs: map[string][]connector.DocumentRef{
			"drive-1":  {{ID: "d1"}},
			"drive-2":  {{ID: "d2"}},
			"shared-1": {{ID: "d3"}},
		},
	}
	t.Run("exact", func(t *testing.T) {
		_, err := bf.Run(context.Background(), pipeline.BackfillRequest{
			TenantID: "t", SourceID: "s",
			Connector: conn, Conn: fakeConn{}, Scopes: []string{"drive-1"},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := cap.snapshot()
		if len(got) != 1 || got[0].DocumentID != "d1" {
			t.Fatalf("expected only d1, got %+v", got)
		}
	})
	t.Run("glob", func(t *testing.T) {
		cap.events = nil
		_, err := bf.Run(context.Background(), pipeline.BackfillRequest{
			TenantID: "t", SourceID: "s",
			Connector: conn, Conn: fakeConn{}, Scopes: []string{"drive-*"},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := cap.snapshot()
		if len(got) != 2 {
			t.Fatalf("glob expected 2, got %d", len(got))
		}
	})
}

func TestBackfill_RatePacingHonoured(t *testing.T) {
	t.Parallel()
	rate := &fakeRate{}
	cap := &captureEmitter{}
	bf, _ := pipeline.NewBackfill(pipeline.BackfillConfig{Producer: cap, Rate: rate})
	conn := &fakeConnector{
		namespaces: []connector.Namespace{{ID: "ns"}},
		docs:       map[string][]connector.DocumentRef{"ns": {{ID: "1"}, {ID: "2"}, {ID: "3"}}},
	}
	if _, err := bf.Run(context.Background(), pipeline.BackfillRequest{
		TenantID: "t", SourceID: "s", Connector: conn, Conn: fakeConn{},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rate.calls != 3 {
		t.Fatalf("Rate.Wait must be called once per document, got %d", rate.calls)
	}
}

func TestBackfill_RateCancellationStopsRun(t *testing.T) {
	t.Parallel()
	rate := &fakeRate{err: context.Canceled}
	cap := &captureEmitter{}
	bf, _ := pipeline.NewBackfill(pipeline.BackfillConfig{Producer: cap, Rate: rate})
	conn := &fakeConnector{
		namespaces: []connector.Namespace{{ID: "ns"}},
		docs:       map[string][]connector.DocumentRef{"ns": {{ID: "1"}, {ID: "2"}}},
	}
	_, err := bf.Run(context.Background(), pipeline.BackfillRequest{
		TenantID: "t", SourceID: "s", Connector: conn, Conn: fakeConn{},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestBackfill_PropagatesEmitError(t *testing.T) {
	t.Parallel()
	cap := &captureEmitter{err: errors.New("kafka down")}
	bf, _ := pipeline.NewBackfill(pipeline.BackfillConfig{Producer: cap})
	conn := &fakeConnector{
		namespaces: []connector.Namespace{{ID: "ns"}},
		docs:       map[string][]connector.DocumentRef{"ns": {{ID: "1"}}},
	}
	if _, err := bf.Run(context.Background(), pipeline.BackfillRequest{
		TenantID: "t", SourceID: "s", Connector: conn, Conn: fakeConn{},
	}); err == nil {
		t.Fatal("expected emit error to propagate")
	}
}

func TestTickerRate_BlocksUntilTick(t *testing.T) {
	t.Parallel()
	r := &pipeline.TickerRate{Per: 10, Window: 50 * time.Millisecond}
	r.Start()
	defer r.Stop()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := r.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Fatalf("ticker exited too fast")
	}
}

func TestTickerRate_NoOpWithoutStart(t *testing.T) {
	t.Parallel()
	r := &pipeline.TickerRate{Per: 10, Window: time.Second}
	if err := r.Wait(context.Background()); err != nil {
		t.Fatalf("uninitialised TickerRate must be a no-op, got %v", err)
	}
}

// fakeRate counts Wait calls and optionally returns a fixed error.
type fakeRate struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRate) Wait(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}
