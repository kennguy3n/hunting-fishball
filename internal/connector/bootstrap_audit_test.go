// Package connector_test — Round-20 Task 13.
//
// Bootstrap-contract audit. The contract: every DeltaSyncer
// connector's `DeltaSync(ctx, conn, ns, "")` (empty cursor)
// returns a non-empty cursor and zero changes — i.e. captures
// the upstream's current high-water mark without backfilling
// the entire history.
//
// Per-connector tests already enforce this for each individual
// surface (TestX_DeltaSync_BootstrapReturnsNow, et al.). This
// file is the meta-audit: it scans the source of every
// connector package and asserts the DeltaSync implementation
// references either an empty-cursor branch (`cursor == ""`,
// `if cursor != ""`, `len(cursor) == 0`) or an inline initial
// token call. Connectors that don't implement DeltaSyncer are
// skipped.
package connector_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bootstrapAuditConnectors mirrors auditedConnectors and adds
// the 1 wrapper (google_shared_drives delegates to googledrive
// via SharedDrivesConnector.DeltaSync, which lives in the
// googledrive package and is already audited there).
//
// Round 20 lifts the floor to 49 first-class connectors (50
// minus the wrapper). Round 24 raises that to 53 by adding
// quip, freshservice, pagerduty, and zoho_desk.
var bootstrapAuditConnectors = []string{
	"airtable", "asana", "azure_blob", "bamboohr", "bitbucket",
	"bookstack", "box", "clickup", "coda", "confluence",
	"confluence_server", "discord", "dropbox", "egnyte",
	"entra_id", "freshdesk", "freshservice", "gcs", "github",
	"gitlab", "gmail", "google_workspace", "googledrive",
	"hubspot", "intercom", "jira", "kchat", "linear",
	"mattermost", "monday", "notion", "okta", "onedrive",
	"outlook", "pagerduty", "personio", "pipedrive", "quip",
	"rss", "s3", "salesforce", "servicenow", "sharepoint",
	"sharepoint_onprem", "sitemap", "teams", "trello",
	"webex", "workday", "zendesk", "zoho_desk",
}

// bootstrapMarkers are the textual signals that a connector's
// DeltaSync handles the empty-cursor case explicitly. The audit
// passes if any one marker is present in the package's
// non-test sources.
var bootstrapMarkers = []string{
	`cursor == ""`,
	`cursor != ""`,
	`len(cursor) == 0`,
	`if cursor ==`,
	`if cursor !=`,
	"initial token",
	"initial cursor",
	"current head",
	"empty cursor",
	"stream_position", // box uses "now" only when cursor == ""
	"deltaToken",      // graph clients
	"deltaLink",       // graph clients
	"high-water",
}

// TestConnectorBootstrap_Audit asserts every DeltaSyncer
// connector source explicitly handles the empty-cursor branch
// (initial token / "now" watermark) rather than backfilling.
func TestConnectorBootstrap_Audit(t *testing.T) {
	t.Parallel()
	for _, name := range bootstrapAuditConnectors {
		dir := name // tests run with cwd = internal/connector
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("audit: read %s: %v", dir, err)
		}
		var src strings.Builder
		hasDeltaSync := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("audit: read %s: %v", e.Name(), err)
			}
			text := string(body)
			if strings.Contains(text, "DeltaSync(") {
				hasDeltaSync = true
			}
			src.WriteString(text)
			src.WriteByte('\n')
		}
		// Connectors without DeltaSync (e.g. slack, upload_portal,
		// teams pre-Round-17) are exempt — they aren't DeltaSyncers.
		if !hasDeltaSync {
			continue
		}
		text := src.String()
		ok := false
		for _, m := range bootstrapMarkers {
			if strings.Contains(text, m) {
				ok = true

				break
			}
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !ok {
				t.Errorf("connector %s implements DeltaSync but does not explicitly handle the empty-cursor bootstrap (no marker matched). The contract is: empty cursor returns the current high-water mark, not a historical backfill.", name)
			}
		})
	}
}
