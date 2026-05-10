package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
	graphragv1 "github.com/kennguy3n/hunting-fishball/proto/graphrag/v1"
)

type fakeGraphRAGClient struct {
	resp     *graphragv1.ExtractResponse
	err      error
	lastReq  *graphragv1.ExtractRequest
	calls    int
	tenantOK bool
}

func (f *fakeGraphRAGClient) ExtractEntities(_ context.Context, req *graphragv1.ExtractRequest) (*graphragv1.ExtractResponse, error) {
	f.calls++
	f.lastReq = req
	if req.TenantId != "" {
		f.tenantOK = true
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

type fakeGraphWriter struct {
	writeCalls  int
	deleteCalls int
	writeNodes  []storage.FalkorNode
	writeEdges  []storage.FalkorEdge
	tenantSeen  string
	docSeen     string
	writeErr    error
	deleteErr   error
}

func (f *fakeGraphWriter) WriteNodes(_ context.Context, tenantID string, nodes []storage.FalkorNode, edges []storage.FalkorEdge) error {
	f.writeCalls++
	f.tenantSeen = tenantID
	f.writeNodes = append(f.writeNodes, nodes...)
	f.writeEdges = append(f.writeEdges, edges...)
	return f.writeErr
}

func (f *fakeGraphWriter) DeleteByDocument(_ context.Context, tenantID, documentID string) error {
	f.deleteCalls++
	f.tenantSeen = tenantID
	f.docSeen = documentID
	return f.deleteErr
}

func TestGraphRAGStage_HappyPath(t *testing.T) {
	t.Parallel()

	client := &fakeGraphRAGClient{
		resp: &graphragv1.ExtractResponse{
			Nodes: []*graphragv1.Node{
				{Id: "n-alice", Label: "Person", Name: "Alice", Mentions: []string{"c1", "c2"}},
				{Id: "n-acme", Label: "Org", Name: "Acme"},
			},
			Edges: []*graphragv1.Edge{
				{SourceId: "n-alice", DestinationId: "n-acme", Relation: "WORKS_AT", Confidence: 0.9},
			},
			ModelId: "regex-v1",
		},
	}
	writer := &fakeGraphWriter{}
	stage := &GraphRAGStageGRPC{Client: client, Writer: writer}

	doc := &Document{TenantID: "tenant-a", DocumentID: "doc-1"}
	blocks := []Block{{BlockID: "c1", Text: "Alice"}, {BlockID: "c2", Text: "Acme"}}
	if err := stage.Enrich(context.Background(), doc, blocks); err != nil {
		t.Fatalf("Enrich error = %v", err)
	}
	if !client.tenantOK {
		t.Fatalf("client did not see tenant id")
	}
	if writer.deleteCalls != 1 || writer.writeCalls != 1 {
		t.Fatalf("delete=%d write=%d, want 1/1", writer.deleteCalls, writer.writeCalls)
	}
	if writer.tenantSeen != "tenant-a" {
		t.Fatalf("writer tenant = %q", writer.tenantSeen)
	}
	if got := len(writer.writeNodes); got != 2 {
		t.Fatalf("nodes = %d, want 2", got)
	}
	if got := writer.writeNodes[0].DocumentID; got != "doc-1" {
		t.Fatalf("node[0].DocumentID = %q", got)
	}
	if got := writer.writeEdges[0].Relation; got != "WORKS_AT" {
		t.Fatalf("edge relation = %q", got)
	}
}

func TestGraphRAGStage_SwallowsClientError(t *testing.T) {
	t.Parallel()

	client := &fakeGraphRAGClient{err: errors.New("rpc unavailable")}
	writer := &fakeGraphWriter{}
	stage := &GraphRAGStageGRPC{Client: client, Writer: writer}

	err := stage.Enrich(context.Background(), &Document{TenantID: "t", DocumentID: "d"},
		[]Block{{BlockID: "c1", Text: "anything"}})
	if err != nil {
		t.Fatalf("Enrich returned error %v; should swallow rpc failure", err)
	}
	if writer.writeCalls != 0 || writer.deleteCalls != 0 {
		t.Fatalf("writer was called despite rpc error: write=%d delete=%d", writer.writeCalls, writer.deleteCalls)
	}
}

func TestGraphRAGStage_NoBlocksIsNoop(t *testing.T) {
	t.Parallel()

	client := &fakeGraphRAGClient{}
	writer := &fakeGraphWriter{}
	stage := &GraphRAGStageGRPC{Client: client, Writer: writer}

	if err := stage.Enrich(context.Background(),
		&Document{TenantID: "t", DocumentID: "d"}, nil); err != nil {
		t.Fatalf("Enrich error = %v", err)
	}
	if client.calls != 0 || writer.writeCalls != 0 {
		t.Fatalf("expected no calls, got client=%d writer.write=%d", client.calls, writer.writeCalls)
	}
}

func TestGraphRAGStage_RejectsMissingTenant(t *testing.T) {
	t.Parallel()

	stage := &GraphRAGStageGRPC{Client: &fakeGraphRAGClient{}, Writer: &fakeGraphWriter{}}
	if err := stage.Enrich(context.Background(),
		&Document{DocumentID: "d"}, []Block{{BlockID: "c1", Text: "x"}}); err == nil {
		t.Fatalf("expected error for missing tenant id")
	}
}

func TestGraphRAGStage_DefaultsLabelAndRelation(t *testing.T) {
	t.Parallel()

	client := &fakeGraphRAGClient{
		resp: &graphragv1.ExtractResponse{
			Nodes: []*graphragv1.Node{{Id: "n1", Label: "", Name: "n1"}},
			Edges: []*graphragv1.Edge{{SourceId: "n1", DestinationId: "n1", Relation: ""}},
		},
	}
	writer := &fakeGraphWriter{}
	stage := &GraphRAGStageGRPC{Client: client, Writer: writer}

	if err := stage.Enrich(context.Background(),
		&Document{TenantID: "t", DocumentID: "d"},
		[]Block{{BlockID: "c1", Text: "x"}}); err != nil {
		t.Fatalf("Enrich error = %v", err)
	}
	if got := writer.writeNodes[0].Label; got != "Entity" {
		t.Fatalf("default label = %q, want Entity", got)
	}
	if got := writer.writeEdges[0].Relation; got != "MENTIONED_WITH" {
		t.Fatalf("default relation = %q", got)
	}
}

func TestNoopGraphRAG_EnrichIsNoop(t *testing.T) {
	t.Parallel()

	if err := graphRAGOrNoop(nil).Enrich(context.Background(), &Document{}, nil); err != nil {
		t.Fatalf("noop returned error: %v", err)
	}
}

// TestGraphRAGStage_DeletePrunesSubgraph locks in that delete /
// purge events propagate through to FalkorDB. Pre-fix the
// coordinator skipped Stage 3b entirely on those events, so the
// only call site for DeleteByDocument was the re-extract path
// inside Enrich — orphan nodes/edges accumulated forever after a
// document was removed from Postgres + Qdrant.
func TestGraphRAGStage_DeletePrunesSubgraph(t *testing.T) {
	t.Parallel()

	writer := &fakeGraphWriter{}
	stage := &GraphRAGStageGRPC{Client: &fakeGraphRAGClient{}, Writer: writer}

	if err := stage.Delete(context.Background(), "tenant-a", "doc-1"); err != nil {
		t.Fatalf("Delete error = %v", err)
	}
	if writer.deleteCalls != 1 {
		t.Fatalf("writer.deleteCalls = %d, want 1", writer.deleteCalls)
	}
	if writer.tenantSeen != "tenant-a" || writer.docSeen != "doc-1" {
		t.Fatalf("writer saw tenant=%q doc=%q", writer.tenantSeen, writer.docSeen)
	}
}

func TestGraphRAGStage_DeleteSwallowsWriterError(t *testing.T) {
	t.Parallel()

	writer := &fakeGraphWriter{deleteErr: errors.New("falkordb unreachable")}
	stage := &GraphRAGStageGRPC{Client: &fakeGraphRAGClient{}, Writer: writer}

	if err := stage.Delete(context.Background(), "t", "d"); err != nil {
		t.Fatalf("Delete must swallow writer error, got %v", err)
	}
	if writer.deleteCalls != 1 {
		t.Fatalf("writer.deleteCalls = %d", writer.deleteCalls)
	}
}

func TestGraphRAGStage_DeleteRejectsMissingArgs(t *testing.T) {
	t.Parallel()

	stage := &GraphRAGStageGRPC{Client: &fakeGraphRAGClient{}, Writer: &fakeGraphWriter{}}
	if err := stage.Delete(context.Background(), "", "d"); err == nil {
		t.Fatalf("expected error for missing tenant id")
	}
	if err := stage.Delete(context.Background(), "t", ""); err == nil {
		t.Fatalf("expected error for missing document id")
	}
	noWriter := &GraphRAGStageGRPC{Client: &fakeGraphRAGClient{}}
	if err := noWriter.Delete(context.Background(), "t", "d"); err == nil {
		t.Fatalf("expected error for missing writer")
	}
}

func TestNoopGraphRAG_DeleteIsNoop(t *testing.T) {
	t.Parallel()

	if err := graphRAGOrNoop(nil).Delete(context.Background(), "t", "d"); err != nil {
		t.Fatalf("noop returned error: %v", err)
	}
}
