// Package runbooks_test pins connector runbook completeness at
// CI time. Round-10 Task 16: every connector registered in the
// process-global connector registry must have a matching
// `docs/runbooks/<name>.md` covering credential rotation,
// quota / rate-limit handling, outage detection, and the known
// error-code surface.
//
// The test blank-imports every connector package the production
// binary blank-imports, then walks the registry. Doing so keeps
// the test honest if a new connector is added: forgetting either
// the runbook or the section makes this test fail.
package runbooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"

	// Blank-imports mirror cmd/api/main.go so the registry is
	// populated before ListSourceConnectors runs.
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/airtable"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/azure_blob"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/bamboohr"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/bitbucket"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/bookstack"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/box"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/coda"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/egnyte"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/entra_id"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/freshdesk"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/freshservice"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gcs"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/google_workspace"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/intercom"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/monday"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/okta"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/outlook"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/pagerduty"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/personio"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/quip"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/rss"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/servicenow"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint_onprem"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sitemap"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/teams"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/trello"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/upload_portal"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/webex"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/workday"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/zendesk"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/zoho_desk"
)

// runbookFilename maps a registry name to its expected runbook
// filename. The registry uses `google_drive` (underscore) while
// the runbook is `googledrive.md`. Strip underscores/hyphens.
//
// Round-15 Task 16: `google_shared_drives` is a thin wrapper
// around the existing googledrive connector (it filters out the
// owner's My Drive). Both registry names share the same runbook
// surface — credential rotation, quotas, outage signals, and
// error codes are identical.
//
// Round-16 Task 17: `confluence_server` is operationally distinct
// from cloud Confluence (separate auth model, on-prem outage
// patterns) and gets its own `confluenceserver.md` runbook via
// the default underscore-stripping mapping.
func runbookFilename(name string) string {
	if name == "google_shared_drives" {
		return "googledrive.md"
	}
	out := strings.ReplaceAll(name, "_", "")
	out = strings.ReplaceAll(out, "-", "")

	return out + ".md"
}

// requiredSections are the substrings every runbook must
// contain. The fuzzy match accepts " Credential rotation",
// "Credential rotation incidents", etc.
var requiredSections = []string{
	"Credential rotation",
	"Quota / rate-limit",
	"Outage detection",
	"error codes",
}

// TestConnectorRunbooks_ExistAndCoverRequiredSections walks the
// connector registry. For each registered connector the test
// asserts (a) the runbook file exists and (b) every section in
// requiredSections appears at least once in the body.
func TestConnectorRunbooks_ExistAndCoverRequiredSections(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) < 54 {
		t.Fatalf("expected at least 54 connectors registered; got %d (%v)", len(names), names)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(".", runbookFilename(name))
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("runbook %s missing: %v", path, err)
			}
			src := string(body)
			missing := []string{}
			for _, sec := range requiredSections {
				if !strings.Contains(src, sec) {
					missing = append(missing, sec)
				}
			}
			if len(missing) > 0 {
				t.Fatalf("runbook %s missing required sections: %v", path, missing)
			}
		})
	}
}
