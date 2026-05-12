// Package connector_test — Round-15 Task 9 audit gate.
//
// This test enforces the Round-15 connector-completeness contract:
// every connector source file under `internal/connector/<name>/`
// must:
//
//  1. Wrap `connector.ErrInvalidConfig` from its Validate path.
//  2. Reference `connector.ErrNotSupported` at least once
//     (for Subscribe / capability gates).
//  3. Handle 429-style rate limiting by surfacing
//     `connector.ErrRateLimited` (or its sentinel name) so the
//     adaptive rate limiter in adaptive_rate.go can react.
//  4. Use `http.NewRequestWithContext` (i.e. respect ctx).
//
// The audit is purely textual — it scans the connector package's
// `.go` source files (excluding `_test.go`). Reviewers should
// treat this as the canonical Phase-7 completeness gate: if a
// connector's source can't satisfy these checks, the registry
// shouldn't accept it.
package connector_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditedConnectors is the canonical Round-15 catalog. Updating
// this list is intentional — every entry must satisfy the audit
// checks below.
var auditedConnectors = []string{
	"asana",
	"box",
	"clickup",
	"confluence",
	"confluence_server",
	"discord",
	"dropbox",
	"github",
	"gitlab",
	"gmail",
	"googledrive",
	"hubspot",
	"jira",
	"kchat",
	"linear",
	"mattermost",
	"monday",
	"notion",
	"okta",
	"onedrive",
	"pipedrive",
	"rss",
	"s3",
	"salesforce",
	"sharepoint",
	"slack",
	"teams",
}

type auditCheck struct {
	name    string
	require string
}

var requiredChecks = []auditCheck{
	{"wraps ErrInvalidConfig", "ErrInvalidConfig"},
	{"references ErrNotSupported", "ErrNotSupported"},
	{"surfaces 429 via ErrRateLimited", "ErrRateLimited"},
	{"uses context-aware HTTP requests", "NewRequestWithContext"},
}

// TestConnectorAudit_Round15 enforces the connector-completeness
// contract first introduced in Round 15 and extended to all 27
// audited connectors (excluding the google_shared_drives wrapper)
// in Round 16.
func TestConnectorAudit_Round15(t *testing.T) {
	t.Parallel()
	for _, name := range auditedConnectors {
		dir := name // tests run with cwd = internal/connector
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("audit: read %s: %v", dir, err)
		}
		var src strings.Builder
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("audit: read %s: %v", e.Name(), err)
			}
			src.Write(body)
			src.WriteByte('\n')
		}
		text := src.String()
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, chk := range requiredChecks {
				if !strings.Contains(text, chk.require) {
					t.Errorf("connector %s fails audit %q: source missing token %q", name, chk.name, chk.require)
				}
			}
		})
	}
}
