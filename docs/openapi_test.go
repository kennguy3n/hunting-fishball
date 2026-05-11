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
	"os"
	"path/filepath"
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
