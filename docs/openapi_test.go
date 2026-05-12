// Package docs_test pins the OpenAPI spec against the actual
// route table registered in cmd/api. Round-9 Task 18: an audit
// surfaced a handful of endpoints that handlers register but
// the spec never documented (retrieval/pins, analytics/queries,
// health/indexes, sync/stream, webhooks, …). This test fails
// when a future code change adds a route without also adding
// it to docs/openapi.yaml.
//
// The test scans the openapi.yaml for the path keys (the lines
// starting with `  /v1/...`) and asserts that every path on
// the must-cover list is present.
package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// requiredPaths is the set of paths the OpenAPI spec MUST
// document. Add to this list when a new handler is registered
// in cmd/api/main.go.
var requiredPaths = []string{
	// Round-1 core surface.
	"/v1/retrieve",
	"/v1/retrieve/batch",
	"/v1/retrieve/explain",
	"/v1/retrieve/stream",
	"/v1/retrieve/feedback",
	"/v1/admin/sources",
	"/v1/admin/audit",
	"/v1/admin/dashboard",
	"/v1/admin/reindex",

	// Round-7 GORM-backed store endpoints + admin surface.
	"/v1/admin/synonyms",
	"/v1/admin/retrieval/experiments",
	"/v1/admin/notifications/subscriptions",
	"/v1/admin/connector-templates",
	"/v1/admin/retrieval/warm-cache",
	"/v1/admin/sources/bulk",
	"/v1/admin/latency-budget",
	"/v1/admin/chunks/quality",
	"/v1/admin/audit/export",
	"/v1/admin/cache-config",
	"/v1/admin/sync-history",
	"/v1/admin/pinned-results",
	"/v1/admin/pipeline/health",
	"/v1/admin/notifications/delivery-log",
	"/v1/admin/sources/{id}/credential-health",

	// Round-9 Task 18 additions (previously undocumented).
	"/v1/admin/sources/{id}/rotate-credentials",
	"/v1/admin/retrieval/pins",
	"/v1/admin/retrieval/pins/{id}",
	"/v1/admin/analytics/queries",
	"/v1/admin/analytics/queries/top",
	"/v1/admin/health/indexes",
	"/v1/admin/sources/{id}/sync/stream",
	"/v1/webhooks/{connector}/{source_id}",
	"/v1/admin/dlq/replay",

	// Round-10 Task 15: complete the spec audit. These were
	// registered by handlers and reachable from gin but never
	// pinned by the test. Once added here, openapi.yaml must
	// keep them in step.
	"/v1/admin/notifications",
	"/v1/admin/notifications/{id}",
	"/v1/admin/connector-templates/{id}",
	"/v1/admin/retrieval/experiments/{name}",
	"/v1/admin/retrieval/experiments/{name}/results",
	"/v1/admin/chunks/quality-report",
	"/v1/admin/sources/{id}/sync-history",
	"/v1/admin/tenants/{id}/cache-config",
	"/v1/admin/tenants/{id}/latency-budget",
	"/v1/admin/tenants/{id}/usage",
	"/v1/admin/tenants/{tenant_id}/export/{job_id}",
	"/v1/admin/sources/{id}/schema",
	"/v1/admin/sources/preview",
	"/v1/admin/sources/{id}/embedding",
	"/v1/admin/isolation-check",
	"/v1/admin/chunks/{chunk_id}",

	// Round-13 additions.
	"/v1/admin/health/summary",
	"/v1/admin/analytics/queries/slow",
	"/v1/admin/analytics/cache-stats",
	"/v1/admin/tenants/{tenant_id}/rotate-api-key",
	"/v1/admin/audit/integrity",

	// Round-14 additions.
	"/v1/admin/pipeline/breakers",
	"/v1/admin/retrieval/latency-histogram",
	"/v1/admin/retrieval/slow-queries",
	"/v1/admin/pipeline/throughput",
}

// TestOpenAPI_RequiredPathsDocumented confirms every path on
// requiredPaths shows up at least once in the openapi.yaml
// file. Whitespace + ordering insensitive.
func TestOpenAPI_RequiredPathsDocumented(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	src := string(data)
	missing := []string{}
	for _, want := range requiredPaths {
		// We look for either `  <path>:` (top-level path entry) or
		// `\n<path>:` to allow tools that re-indent the YAML. A bare
		// strings.Contains on the path string is the simplest sound
		// check and matches the manifest pattern used elsewhere in
		// the repo.
		needle := want + ":"
		if !strings.Contains(src, needle) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("openapi.yaml: missing path entries: %v", missing)
	}
}

// TestOpenAPI_KindIsOpenAPI3 sanity check on the document's
// top-level openapi declaration.
func TestOpenAPI_KindIsOpenAPI3(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	if !strings.Contains(string(data), "openapi: 3.") {
		t.Fatal("openapi.yaml: missing `openapi: 3.x` declaration")
	}
}

// routeFromCall mirrors adminRouteFromCall in
// tests/integration/rbac_coverage_test.go but matches any
// /v1/ prefix (admin + public). Returns (method, path) on
// success, "", "", false otherwise.
func routeFromCall(call *ast.CallExpr) (string, string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	method := sel.Sel.Name
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return "", "", false
	}
	if len(call.Args) == 0 {
		return "", "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok {
		return "", "", false
	}
	val := strings.Trim(lit.Value, `"`)
	if !strings.HasPrefix(val, "/v1/") {
		return "", "", false
	}
	return method, val, true
}

// normalizeGinPathParam rewrites a gin path param (":id") to the
// OpenAPI form ("{id}") so the two manifests are comparable.
func normalizeGinPathParam(p string) string {
	re := regexp.MustCompile(`:([a-zA-Z_][a-zA-Z0-9_]*)`)
	return re.ReplaceAllString(p, "{$1}")
}

// pathsFromOpenAPI extracts every top-level path key from the
// openapi.yaml so we can compare against the discovered route
// manifest. We do a light-weight scan rather than a full YAML
// parse because the spec only has one path-key indentation level.
func pathsFromOpenAPI(t *testing.T, src string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	inPaths := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if inPaths && len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			// Left a paths block.
			break
		}
		if !inPaths {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") || !strings.HasSuffix(trimmed, ":") {
			continue
		}
		// Path keys in openapi.yaml are indented by two spaces.
		if !strings.HasPrefix(line, "  /") {
			continue
		}
		path := strings.TrimSuffix(trimmed, ":")
		out[path] = struct{}{}
	}
	return out
}

// TestOpenAPI_RouterCoverage walks internal/admin/*.go +
// internal/audit/*.go for gin route registrations under /v1/ and
// asserts every registered route has a corresponding path entry
// in docs/openapi.yaml. Round-13 Task 17. This catches new
// endpoints shipping without OpenAPI documentation.
//
// The test runs in the fast lane (no docker, no real router).
func TestOpenAPI_RouterCoverage(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join(".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	dirs := []string{
		filepath.Join(repoRoot, "internal", "admin"),
		filepath.Join(repoRoot, "internal", "audit"),
		filepath.Join(repoRoot, "internal", "retrieval"),
	}
	fset := token.NewFileSet()
	routes := map[string]struct{}{}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
				strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			file, ferr := parser.ParseFile(fset, filepath.Join(d, e.Name()), nil, parser.AllErrors)
			if ferr != nil {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				_, p, ok := routeFromCall(call)
				if !ok {
					return true
				}
				routes[normalizeGinPathParam(p)] = struct{}{}
				return true
			})
		}
	}
	if len(routes) < 10 {
		t.Fatalf("router scan returned %d routes; scanner is broken", len(routes))
	}

	data, err := os.ReadFile(filepath.Join(".", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	documented := pathsFromOpenAPI(t, string(data))

	// Allowlist: a handful of registered routes are intentionally
	// undocumented because they are internal/no-stable-contract
	// (e.g. test-only debug helpers). Add a path here only after
	// a deliberate decision NOT to expose it in the public spec.
	allow := map[string]struct{}{}

	var missing []string
	for p := range routes {
		if _, ok := allow[p]; ok {
			continue
		}
		if _, ok := documented[p]; ok {
			continue
		}
		missing = append(missing, p)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("openapi.yaml is missing %d routes registered in the router:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("verified %d registered /v1/ routes against openapi.yaml", len(routes))
}
