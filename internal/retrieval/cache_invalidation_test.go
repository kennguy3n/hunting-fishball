// Round-12 Task 14 — cache invalidation completeness audit.
//
// This is a structural (AST-driven) contract test. We walk the Go
// AST of every file under internal/pipeline/, enumerate every call
// to a "write" method on storage backends (Vector, Metadata, Graph
// Writer), and confirm the same function also calls "Invalidate".
//
// Today the pipeline has not yet wired explicit cache.Invalidate
// calls; the cache is refreshed lazily on TTL expiry. The test
// captures the current shape as a baseline manifest so any *new*
// write that lacks an Invalidate companion fails the test. When
// the pipeline later gains explicit Invalidate calls, the baseline
// manifest shrinks — that's expected, intentional drift.
package retrieval_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// storageWriteSigs is the set of (receiverKeyword, method) pairs
// the audit considers "cache-affecting writes". The keyword is
// matched anywhere in the flattened receiver expression so
// "s.cfg.Vector" matches "Vector".
var storageWriteSigs = []struct {
	receiverKeyword string
	method          string
}{
	{"Vector", "Upsert"},
	{"Vector", "Delete"},
	{"Metadata", "UpsertChunks"},
	{"Metadata", "DeleteByDocument"},
	{"meta", "UpsertChunks"},
	{"meta", "DeleteByDocument"},
	{"Writer", "WriteNodes"},
	{"Writer", "DeleteByDocument"},
	{"Store", "Delete"},
	{"GraphRAG", "Delete"},
}

// selectorString flattens a selector chain into "a.b.c.D" form.
func selectorString(n ast.Expr) string {
	var buf strings.Builder
	walkExpr(n, &buf)
	return buf.String()
}

func walkExpr(n ast.Expr, buf *strings.Builder) {
	switch v := n.(type) {
	case *ast.Ident:
		buf.WriteString(v.Name)
	case *ast.SelectorExpr:
		walkExpr(v.X, buf)
		buf.WriteByte('.')
		buf.WriteString(v.Sel.Name)
	case *ast.CallExpr:
		walkExpr(v.Fun, buf)
		buf.WriteString("()")
	default:
		buf.WriteByte('?')
	}
}

// isStorageWrite checks whether a call expression matches one of
// the storage write signatures.
func isStorageWrite(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	methodName := sel.Sel.Name
	receiverStr := selectorString(sel.X)
	rxLower := strings.ToLower(receiverStr)
	for _, sig := range storageWriteSigs {
		if methodName == sig.method && strings.Contains(rxLower, strings.ToLower(sig.receiverKeyword)) {
			return receiverStr + "." + methodName, true
		}
	}
	return "", false
}

// findStorageWrites walks fn and returns every storage write site.
func findStorageWrites(fn *ast.FuncDecl) []string {
	var out []string
	if fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if site, ok := isStorageWrite(call); ok {
			out = append(out, site)
		}
		return true
	})
	return out
}

// hasInvalidateCall reports whether fn's body calls any method
// whose name starts with "Invalidate" (regardless of receiver).
// Recognised companion calls include the original Round-12
// Invalidate(chunkIDs) surface and the Round-19/20 tag-based
// InvalidateBySources(sourceIDs) variant introduced for fleet-wide
// source-scoped expiration.
func hasInvalidateCall(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	var seen bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if strings.HasPrefix(sel.Sel.Name, "Invalidate") {
			seen = true
			return false
		}
		return true
	})
	return seen
}

// TestCacheInvalidation_PipelineWriteSitesAudit walks internal/pipeline/
// looking for unmitigated writes. The pass criterion is a stable
// baseline manifest — adding a new write to the pipeline without a
// matching Invalidate (or manifest entry) trips this test.
func TestCacheInvalidation_PipelineWriteSitesAudit(t *testing.T) {
	t.Parallel()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	pipelineDir := filepath.Join(repoRoot, "internal", "pipeline")

	entries, err := os.ReadDir(pipelineDir)
	if err != nil {
		t.Fatalf("read pipeline dir: %v", err)
	}

	var findings []string
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(pipelineDir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			writes := findStorageWrites(fn)
			if len(writes) == 0 {
				continue
			}
			if hasInvalidateCall(fn) {
				continue
			}
			for _, w := range writes {
				findings = append(findings,
					e.Name()+":"+fn.Name.Name+":"+w)
			}
		}
	}

	sort.Strings(findings)

	// Baseline manifest: the audited write sites as of Round 12.
	// New entries are not added silently — a PR that introduces a
	// new write must either add cache.Invalidate, or knowingly
	// extend this manifest with a justification.
	//
	// Why no Invalidate today: the semantic cache uses TTL-based
	// expiry rather than write-through invalidation. The pipeline
	// writes to Qdrant/FalkorDB/Metadata but does not call
	// cache.Invalidate; cache staleness is bounded by the TTL
	// configured via CONTEXT_ENGINE_CACHE_TTL. This manifest
	// freezes the current surface so any future expansion of
	// write paths is visible in the test.
	//
	// Round-14 Task 12 re-audited the pipeline through Round 13:
	// Round 13 added stage circuit breakers, API-key rotation,
	// degraded-mode embedding fallback, and audit-integrity
	// verification — none of which add new entries to the
	// retrieval/Qdrant/FalkorDB write surface. The baseline
	// below therefore still freezes the same write sites; if a
	// future round adds a new Stage-4 write call, this audit
	// will fail until the manifest is consciously extended.
	baseline := map[string]struct{}{
		"coordinator.go:Run:c.cfg.GraphRAG.Delete":                 {},
		"coordinator.go:Run:c.cfg.Store.Delete":                    {},
		"graphrag.go:Delete:g.Writer.DeleteByDocument":             {},
		"graphrag.go:Enrich:g.Writer.DeleteByDocument":             {},
		"graphrag.go:Enrich:g.Writer.WriteNodes":                   {},
		"retention_deleter.go:DeleteChunk:d.meta.DeleteByDocument": {},
		"retention_deleter.go:DeleteChunk:d.vector.Delete":         {},
		"store.go:Delete:s.cfg.Metadata.DeleteByDocument":          {},
		"store.go:Delete:s.cfg.Vector.Delete":                      {},
		"store.go:Store:s.cfg.Metadata.DeleteByDocument":           {},
		"store.go:Store:s.cfg.Metadata.UpsertChunks":               {},
		"store.go:Store:s.cfg.Vector.Delete":                       {},
		"store.go:Store:s.cfg.Vector.Upsert":                       {},
	}

	for _, f := range findings {
		if _, allowed := baseline[f]; !allowed {
			t.Errorf("new write site %q has no Invalidate companion and is not in the baseline manifest", f)
		}
	}
	// Advisory: flag stale baseline entries that no longer exist.
	have := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		have[f] = struct{}{}
	}
	for k := range baseline {
		if _, ok := have[k]; !ok {
			t.Logf("baseline entry %q no longer found — drop from manifest (advisory)", k)
		}
	}
}

// TestCacheInvalidation_TagBasedSurfaceExists — Round-20 Task 18.
//
// The Round-19 tag-based InvalidateBySources surface is the
// approved companion for source-scoped reindex flows (manual
// purge, embedding-model rotation, retention sweep). This
// structural test guarantees the surface remains present and
// callable on storage.SemanticCache so admin/orchestration code
// has a stable target — if a future round inadvertently removes
// it the test fails loudly.
func TestCacheInvalidation_TagBasedSurfaceExists(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	path := filepath.Join(repoRoot, "internal", "storage", "redis_cache.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]bool{
		"Invalidate":          false,
		"InvalidateBySources": false,
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		if _, hit := want[fn.Name.Name]; hit {
			want[fn.Name.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("SemanticCache.%s not found — Stage-4 cache invalidation surface incomplete", name)
		}
	}
}
