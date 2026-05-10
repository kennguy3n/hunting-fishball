package pipeline

// Phase 3 Stage 3b: GraphRAG entity-extraction hook.
//
// The coordinator calls Enrich after embedding (Stage 3) and before
// store (Stage 4). The hook is opt-in per ARCHITECTURE.md §3.3:
// when CoordinatorConfig.GraphRAG is nil the coordinator is a no-op
// 4-stage pipeline. When it's non-nil the coordinator pushes
// extracted nodes + edges into a per-tenant graph backend
// (FalkorDB in production) so retrieval can issue traversal queries
// against the same chunk corpus.
//
// Failure isolation. The graph write is best-effort: a GraphRAG
// outage MUST NOT block ingestion. The coordinator records a
// metrics counter and an audit log entry, then continues to Stage 4.
// Operators monitor the counter to detect protracted GraphRAG
// failures (Phase 8 alerting rules).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
	graphragv1 "github.com/kennguy3n/hunting-fishball/proto/graphrag/v1"
)

// GraphRAGStage is the abstract Stage 3b contract. The production
// implementation (GraphRAGStageGRPC) calls the Python service
// + the FalkorDB graph writer; tests inject a fake.
type GraphRAGStage interface {
	// Enrich extracts entities + relations from the supplied
	// document/blocks and persists them into the per-tenant graph.
	// Implementations MUST return nil when graph writes are
	// best-effort and failures should not surface to the
	// coordinator. Returning a non-nil error is reserved for
	// programmer errors (nil arguments, missing tenant scope) so
	// the coordinator can log them but continue.
	Enrich(ctx context.Context, doc *Document, blocks []Block) error
}

// GraphRAGClient is the narrow contract this package needs from a
// graphragv1 gRPC client. It exists so tests can inject a fake
// without dragging the generated stub's full surface in.
type GraphRAGClient interface {
	ExtractEntities(
		ctx context.Context,
		req *graphragv1.ExtractRequest,
	) (*graphragv1.ExtractResponse, error)
}

// GraphWriter is the storage-side contract — it accepts pre-mapped
// FalkorNode/FalkorEdge slices keyed by tenant. *storage.FalkorDBClient
// satisfies it directly.
type GraphWriter interface {
	WriteNodes(ctx context.Context, tenantID string, nodes []storage.FalkorNode, edges []storage.FalkorEdge) error
	DeleteByDocument(ctx context.Context, tenantID, documentID string) error
}

// GraphRAGStageGRPC is the production GraphRAGStage. It calls a
// remote graphragv1 service for extraction and writes the result
// into a tenant graph through GraphWriter.
type GraphRAGStageGRPC struct {
	Client  GraphRAGClient
	Writer  GraphWriter
	ModelID string
	Logger  *slog.Logger
}

// Enrich runs the full Stage 3b pipeline:
//
//  1. project the doc + blocks into an ExtractRequest;
//  2. RPC to the GraphRAG service;
//  3. delete-then-write the per-document subgraph into FalkorDB so
//     re-ingestion of the same document supersedes prior nodes.
//
// Errors are logged but always returned as nil so the caller never
// blocks ingestion on graph-side failures.
func (g *GraphRAGStageGRPC) Enrich(ctx context.Context, doc *Document, blocks []Block) error {
	if doc == nil {
		return errors.New("graphrag: nil document")
	}
	if doc.TenantID == "" {
		return errors.New("graphrag: missing tenant id")
	}
	if g.Client == nil || g.Writer == nil {
		return errors.New("graphrag: missing client or writer")
	}
	if len(blocks) == 0 {
		return nil
	}

	chunks := make([]*graphragv1.Chunk, 0, len(blocks))
	for _, b := range blocks {
		chunks = append(chunks, &graphragv1.Chunk{
			ChunkId: b.BlockID,
			Text:    b.Text,
		})
	}

	resp, err := g.Client.ExtractEntities(ctx, &graphragv1.ExtractRequest{
		TenantId:   doc.TenantID,
		DocumentId: doc.DocumentID,
		Chunks:     chunks,
		ModelId:    g.ModelID,
	})
	if err != nil {
		g.log().Warn("graphrag extract failed",
			"tenant", doc.TenantID,
			"document", doc.DocumentID,
			"error", err.Error(),
		)
		return nil
	}

	// Re-extract the document → delete prior subgraph then write
	// the new one in one logical operation. Both calls are
	// idempotent so a partial failure on the second call is
	// recoverable on the next ingestion event.
	if err := g.Writer.DeleteByDocument(ctx, doc.TenantID, doc.DocumentID); err != nil {
		g.log().Warn("graphrag prune prior subgraph failed",
			"tenant", doc.TenantID,
			"document", doc.DocumentID,
			"error", err.Error(),
		)
		return nil
	}

	nodes, edges := projectGraphPayload(doc.DocumentID, resp)
	if len(nodes) == 0 && len(edges) == 0 {
		return nil
	}
	if err := g.Writer.WriteNodes(ctx, doc.TenantID, nodes, edges); err != nil {
		g.log().Warn("graphrag write subgraph failed",
			"tenant", doc.TenantID,
			"document", doc.DocumentID,
			"nodes", len(nodes),
			"edges", len(edges),
			"error", err.Error(),
		)
	}
	return nil
}

func (g *GraphRAGStageGRPC) log() *slog.Logger {
	if g.Logger != nil {
		return g.Logger
	}
	return observability.NewLogger("graphrag-stage")
}

func projectGraphPayload(documentID string, resp *graphragv1.ExtractResponse) ([]storage.FalkorNode, []storage.FalkorEdge) {
	if resp == nil {
		return nil, nil
	}
	nodes := make([]storage.FalkorNode, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		props := map[string]any{}
		props["name"] = n.Name
		if len(n.Mentions) > 0 {
			// FalkorDB property values are scalar; fold the
			// mention list into a comma-joined string so we
			// can still answer "which chunks mention X" via a
			// substring match.
			props["mentions"] = joinNonEmpty(n.Mentions, ",")
		}
		for k, v := range n.Properties {
			if k == "name" || k == "mentions" || k == "id" {
				continue
			}
			props[k] = v
		}
		nodes = append(nodes, storage.FalkorNode{
			ID:         n.Id,
			Label:      labelFor(n.Label),
			DocumentID: documentID,
			Properties: props,
		})
	}
	edges := make([]storage.FalkorEdge, 0, len(resp.Edges))
	for _, e := range resp.Edges {
		props := map[string]any{}
		if e.Confidence > 0 {
			props["confidence"] = float64(e.Confidence)
		}
		for k, v := range e.Properties {
			if k == "confidence" {
				continue
			}
			props[k] = v
		}
		edges = append(edges, storage.FalkorEdge{
			Source:      e.SourceId,
			Destination: e.DestinationId,
			Relation:    relationFor(e.Relation),
			DocumentID:  documentID,
			Properties:  props,
		})
	}
	return nodes, edges
}

// labelFor returns a non-empty FalkorDB label, defaulting to
// "Entity" if the extractor returned blank.
func labelFor(label string) string {
	if label == "" {
		return "Entity"
	}
	return label
}

// relationFor returns a non-empty edge relation, defaulting to
// "MENTIONED_WITH" if the extractor returned blank.
func relationFor(rel string) string {
	if rel == "" {
		return "MENTIONED_WITH"
	}
	return rel
}

func joinNonEmpty(xs []string, sep string) string {
	first := true
	out := ""
	for _, x := range xs {
		if x == "" {
			continue
		}
		if !first {
			out += sep
		}
		out += x
		first = false
	}
	return out
}

// noopGraphRAG is the implementation used when the coordinator is
// configured without a GraphRAG hook. Keeps the stage-3b call site
// branch-free.
type noopGraphRAG struct{}

func (noopGraphRAG) Enrich(_ context.Context, _ *Document, _ []Block) error { return nil }

// graphRAGOrNoop hands back a non-nil stage so the coordinator never
// has to nil-check inside the hot path.
func graphRAGOrNoop(s GraphRAGStage) GraphRAGStage {
	if s == nil {
		return noopGraphRAG{}
	}
	return s
}

// fmtErr is unused outside this package but kept here as a single
// place to format graphrag errors when callers want to lift them.
func fmtErr(stage string, err error) error { return fmt.Errorf("graphrag %s: %w", stage, err) }

var _ = fmtErr // keep linter happy until callers consume it.
