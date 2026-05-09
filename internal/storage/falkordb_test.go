package storage_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// fakeFalkorCmd is the per-command stub returned by fakeFalkorDB.Do.
type fakeFalkorCmd struct {
	val any
	err error
}

func (c *fakeFalkorCmd) Result() (any, error) { return c.val, c.err }
func (c *fakeFalkorCmd) Err() error           { return c.err }

// fakeFalkorDB is the in-memory FalkorDB stand-in used by unit tests.
// It records every Do(args...) the client issues so tests can assert
// on graph naming, query shape, and per-tenant isolation.
type fakeFalkorDB struct {
	mu    sync.Mutex
	calls [][]any

	// Programmable response: returnVal is wrapped into the next
	// fakeFalkorCmd; returnErr is propagated as the cmd's Err.
	returnVal any
	returnErr error
}

func (f *fakeFalkorDB) Do(_ context.Context, args ...any) storage.FalkorCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)

	return &fakeFalkorCmd{val: f.returnVal, err: f.returnErr}
}

func (f *fakeFalkorDB) lastQuery(t *testing.T) (graph, cypher string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatalf("no Do calls recorded")
	}
	last := f.calls[len(f.calls)-1]
	if len(last) != 3 {
		t.Fatalf("expected GRAPH.QUERY with 3 args, got %d: %v", len(last), last)
	}
	g, _ := last[1].(string)
	c, _ := last[2].(string)

	return g, c
}

func newFalkorClient(t *testing.T, fake *fakeFalkorDB) *storage.FalkorDBClient {
	t.Helper()
	c, err := storage.NewFalkorDBClient(storage.FalkorDBConfig{Client: fake, GraphPrefix: "hf-"})
	if err != nil {
		t.Fatalf("NewFalkorDBClient: %v", err)
	}

	return c
}

func TestFalkorDB_NewClient_RequiresClient(t *testing.T) {
	t.Parallel()

	_, err := storage.NewFalkorDBClient(storage.FalkorDBConfig{})
	if err == nil {
		t.Fatalf("expected error when Client nil")
	}
}

func TestFalkorDB_TenantScopeGuards(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)
	ctx := context.Background()

	if err := c.WriteNodes(ctx, "", []storage.FalkorNode{{ID: "n", Label: "X"}}, nil); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("WriteNodes: %v", err)
	}
	if _, err := c.Traverse(ctx, "", "MATCH (n) RETURN n", 5); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Traverse: %v", err)
	}
	if err := c.DeleteByDocument(ctx, "", "doc"); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("DeleteByDocument: %v", err)
	}
}

func TestFalkorDB_GraphFor_PerTenant(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)
	if got := c.GraphFor("tenant-a"); got != "hf-tenant-a" {
		t.Fatalf("graph: %q", got)
	}
}

func TestFalkorDB_WriteNodes_Cypher(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)

	if err := c.WriteNodes(context.Background(), "tenant-a",
		[]storage.FalkorNode{
			{
				ID:         "n1",
				Label:      "Document",
				DocumentID: "doc-1",
				Properties: map[string]any{"title": "Q3 report"},
			},
		},
		[]storage.FalkorEdge{
			{Source: "n1", Destination: "n2", Relation: "MENTIONS", DocumentID: "doc-1"},
		},
	); err != nil {
		t.Fatalf("WriteNodes: %v", err)
	}

	graph, cypher := fake.lastQuery(t)
	if graph != "hf-tenant-a" {
		t.Fatalf("graph: %q", graph)
	}
	for _, fragment := range []string{"MERGE (n0:Document", "id:\"n1\"", "MERGE (s0 {id:\"n1\"})", "MERGE (d0 {id:\"n2\"})", "r0:MENTIONS"} {
		if !strings.Contains(cypher, fragment) {
			t.Fatalf("expected cypher to contain %q, got:\n%s", fragment, cypher)
		}
	}
}

func TestFalkorDB_WriteNodes_RejectsBadInput(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)
	ctx := context.Background()

	if err := c.WriteNodes(ctx, "tenant-a", []storage.FalkorNode{{ID: "", Label: "X"}}, nil); err == nil {
		t.Fatalf("expected error for missing ID")
	}
	if err := c.WriteNodes(ctx, "tenant-a", []storage.FalkorNode{{ID: "n", Label: ""}}, nil); err == nil {
		t.Fatalf("expected error for missing Label")
	}
	if err := c.WriteNodes(ctx, "tenant-a", nil, []storage.FalkorEdge{{Source: "", Destination: "n", Relation: "X"}}); err == nil {
		t.Fatalf("expected error for missing edge source")
	}
}

func TestFalkorDB_WriteNodes_NoOpOnEmpty(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)
	if err := c.WriteNodes(context.Background(), "tenant-a", nil, nil); err != nil {
		t.Fatalf("WriteNodes empty: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 0 {
		t.Fatalf("expected no Do calls, got %d", len(fake.calls))
	}
}

func TestFalkorDB_Traverse_AppendsLimit(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{
		// `[ headers, rows, statistics ]` — one row whose first cell
		// is a node `[ id, [labels], [ [propname, value], ... ] ]`.
		returnVal: []any{
			[]any{"n"},
			[]any{
				[]any{
					[]any{
						"node-1",
						[]any{"Document"},
						[]any{
							[]any{"id", "node-1"},
							[]any{"document_id", "doc-1"},
						},
					},
				},
			},
			[]any{"Query internal execution time: 1ms"},
		},
	}
	c := newFalkorClient(t, fake)

	matches, err := c.Traverse(context.Background(), "tenant-a", "MATCH (n:Document) RETURN n", 25)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches: %d", len(matches))
	}
	if matches[0].NodeID != "node-1" || matches[0].DocumentID != "doc-1" || matches[0].Label != "Document" {
		t.Fatalf("match: %+v", matches[0])
	}

	_, cypher := fake.lastQuery(t)
	if !strings.Contains(cypher, "LIMIT 25") {
		t.Fatalf("expected LIMIT 25, got %s", cypher)
	}
}

func TestFalkorDB_Traverse_ParsesFlatRow(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{
		returnVal: []any{
			[]any{"n.id", "n.label", "score"},
			[]any{
				[]any{"node-1", "Document", float64(0.9)},
			},
			[]any{"stats"},
		},
	}
	c := newFalkorClient(t, fake)

	matches, err := c.Traverse(context.Background(), "tenant-a", "MATCH (n) RETURN n.id, n.label, 0.9 AS score", 5)
	if err != nil {
		t.Fatalf("Traverse: %v", err)
	}
	if len(matches) != 1 || matches[0].NodeID != "node-1" || matches[0].Label != "Document" || matches[0].Score != 0.9 {
		t.Fatalf("match: %+v", matches[0])
	}
}

func TestFalkorDB_Traverse_HandlesError(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{returnErr: errors.New("network down")}
	c := newFalkorClient(t, fake)
	_, err := c.Traverse(context.Background(), "tenant-a", "MATCH (n) RETURN n", 5)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestFalkorDB_DeleteByDocument(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)

	if err := c.DeleteByDocument(context.Background(), "tenant-a", "doc-1"); err != nil {
		t.Fatalf("DeleteByDocument: %v", err)
	}
	graph, cypher := fake.lastQuery(t)
	if graph != "hf-tenant-a" {
		t.Fatalf("graph: %q", graph)
	}
	if !strings.Contains(cypher, "{document_id:\"doc-1\"}") || !strings.Contains(cypher, "DETACH DELETE") {
		t.Fatalf("cypher: %s", cypher)
	}

	if err := c.DeleteByDocument(context.Background(), "tenant-a", ""); err == nil {
		t.Fatalf("expected error for empty docID")
	}
}

func TestFalkorDB_TenantsCallSeparateGraphs(t *testing.T) {
	t.Parallel()

	fake := &fakeFalkorDB{}
	c := newFalkorClient(t, fake)
	ctx := context.Background()

	if err := c.WriteNodes(ctx, "tenant-a", []storage.FalkorNode{{ID: "n", Label: "X"}}, nil); err != nil {
		t.Fatalf("a: %v", err)
	}
	if err := c.WriteNodes(ctx, "tenant-b", []storage.FalkorNode{{ID: "n", Label: "X"}}, nil); err != nil {
		t.Fatalf("b: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fake.calls))
	}
	if fake.calls[0][1] != "hf-tenant-a" || fake.calls[1][1] != "hf-tenant-b" {
		t.Fatalf("graphs: %v / %v", fake.calls[0][1], fake.calls[1][1])
	}
}
