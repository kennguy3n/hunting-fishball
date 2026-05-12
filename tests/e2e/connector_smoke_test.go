//go:build e2e

// Package e2e — connector_smoke_test exercises every registered
// connector against a local httptest mock of its upstream API. The
// test is independent of the docker-compose storage plane; it only
// verifies the connector layer's contract:
//
//   - Validate accepts the well-formed config.
//   - Connect → ListNamespaces → ListDocuments → FetchDocument round-trips
//     through the mock without errors.
//   - For DeltaSyncer connectors, DeltaSync returns a fresh cursor.
//   - For WebhookReceiver connectors, HandleWebhook decodes a sample
//     payload into at least one DocumentChange.
//   - The process-global connector registry has exactly 20 entries
//     after blank-imports complete (Round-15 catalog expansion).
//
// Run via `make test-connector-smoke` (or as part of `make test-e2e`).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"

	// Blank-import every connector so the registry self-populates.
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/box"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/teams"

	// Direct imports so each smoke case can call the connector's
	// `New(With…)` option helpers without going through the registry's
	// stripped-down factory.
	"github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	"github.com/kennguy3n/hunting-fishball/internal/connector/box"
	"github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	"github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	"github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	gh "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	"github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	"github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	"github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	"github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	"github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	"github.com/kennguy3n/hunting-fishball/internal/connector/teams"
)

// expectedConnectors is the canonical list of registered connector
// names. The smoke suite is the on-call's check that no connector
// silently disappears from a binary's blank-import list.
var expectedConnectors = []string{
	"asana",
	"box",
	"confluence",
	"discord",
	"dropbox",
	"github",
	"gitlab",
	"google_drive",
	"google_shared_drives",
	"hubspot",
	"jira",
	"kchat",
	"linear",
	"notion",
	"onedrive",
	"s3",
	"salesforce",
	"sharepoint",
	"slack",
	"teams",
}

func TestConnectorSmoke_RegistryHasAllConnectors(t *testing.T) {
	t.Parallel()
	got := connector.ListSourceConnectors()
	sort.Strings(got)
	want := append([]string(nil), expectedConnectors...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("registered connectors: got %d (%v) want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered connectors mismatch: got %v want %v", got, want)
		}
	}
}

// runListNamespacesAndFetch is a shared helper for connectors whose
// ListNamespaces returns at least one namespace. It does the
// Connect → ListNamespaces → ListDocuments → FetchDocument walk and
// asserts none of the steps error.
func runListNamespacesAndFetch(
	t *testing.T,
	c connector.SourceConnector,
	cfg connector.ConnectorConfig,
) (connector.Connection, []connector.Namespace) {
	t.Helper()
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(ns) == 0 {
		t.Fatalf("expected at least one namespace")
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; iterator err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	return conn, ns
}

// fetchOnly wires a connector with a synthetic Namespace (no
// ListNamespaces support / not needed for the smoke).
func fetchOnly(
	t *testing.T,
	c connector.SourceConnector,
	cfg connector.ConnectorConfig,
	ns connector.Namespace,
) connector.Connection {
	t.Helper()
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, err := c.ListDocuments(ctx, conn, ns, connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; iterator err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	return conn
}

// ---- per-connector smoke tests ----------------------------------------

func TestConnectorSmoke_GoogleDrive(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/drives", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"drives":[{"id":"D1","name":"Engineering"}]}`)
	})
	mux.HandleFunc("/files", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"files":[{"id":"f1","name":"hello.txt","modifiedTime":"2025-01-01T00:00:00Z","version":"1"}]}`)
	})
	mux.HandleFunc("/changes/startPageToken", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"startPageToken":"token-1"}`)
	})
	mux.HandleFunc("/changes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"changes":[{"fileId":"f1","removed":false,"file":{"id":"f1"}}],"newStartPageToken":"token-2"}`)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = io.WriteString(w, "hello drive")
			return
		}
		_, _ = io.WriteString(w, `{"id":"f1","name":"hello.txt","mimeType":"text/plain","size":"11","modifiedTime":"2025-01-01T00:00:00Z","createdTime":"2024-12-01T00:00:00Z","owners":[{"emailAddress":"a@b","displayName":"A"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googledrive.New(googledrive.WithBaseURL(srv.URL), googledrive.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(googledrive.Credentials{AccessToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "my-drive"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Slack(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"team_id":"T1"}`)
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"general","is_private":false}]}`)
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"messages":[{"ts":"1700000000.000100","user":"U1","text":"hello"}],"has_more":false}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := slack.New(slack.WithBaseURL(srv.URL), slack.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(slack.Credentials{BotToken: "xoxb-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	runListNamespacesAndFetch(t, c, cfg)
	// Slack is a WebhookReceiver, not a DeltaSyncer.
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"type":"event_callback","event":{"type":"message","channel":"C1","ts":"1700000000.000100","user":"U1","text":"hi"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_SharePoint(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case r.URL.Path == "/sites":
			_, _ = io.WriteString(w, `{"value":[{"id":"site-1","displayName":"Eng","webUrl":"https://x"}]}`)
		case strings.HasSuffix(r.URL.Path, "/drive/root/children"):
			_, _ = io.WriteString(w, `{"value":[{"id":"f1","name":"a.docx","eTag":"etag1","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/drive/items/f1"):
			_, _ = io.WriteString(w, `{"id":"f1","name":"a.docx","size":42,"file":{"mimeType":"text/plain"}}`)
		case strings.HasSuffix(r.URL.Path, "/drive/items/f1/content"):
			_, _ = io.WriteString(w, "hello")
		case strings.HasSuffix(r.URL.Path, "/drive/root/delta"):
			_, _ = io.WriteString(w, `{"value":[{"id":"a","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],"@odata.deltaLink":"NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepoint.New(sharepoint.WithBaseURL(srv.URL), sharepoint.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "site-1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_OneDrive(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/root/children"):
			_, _ = io.WriteString(w, `{"value":[{"id":"f1","name":"a","eTag":"e","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/items/f1"):
			_, _ = io.WriteString(w, `{"name":"a","size":1,"file":{"mimeType":"text/plain"}}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/items/f1/content"):
			_, _ = io.WriteString(w, "hi")
		case strings.HasSuffix(r.URL.Path, "/me/drive/root/delta"):
			_, _ = io.WriteString(w, `{"value":[{"id":"a","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],"@odata.deltaLink":"NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := onedrive.New(onedrive.WithBaseURL(srv.URL), onedrive.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "me"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Dropbox(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/get_current_account":
			_, _ = io.WriteString(w, `{"account_id":"u"}`)
		case "/files/list_folder":
			_, _ = io.WriteString(w, `{"entries":[{".tag":"file","id":"id:1","name":"a","rev":"r","server_modified":"2024-01-01T00:00:00Z"}],"cursor":"CUR","has_more":false}`)
		case "/files/download":
			w.Header().Set("Dropbox-API-Result", `{"name":"a","size":2,"server_modified":"2024-01-01T00:00:00Z"}`)
			_, _ = io.WriteString(w, "hi")
		case "/files/list_folder/get_latest_cursor":
			_, _ = io.WriteString(w, `{"cursor":"CUR0"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := dropbox.New(
		dropbox.WithBaseURL(srv.URL),
		dropbox.WithContentURL(srv.URL),
		dropbox.WithHTTPClient(srv.Client()),
	)
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	// Dropbox ListNamespaces returns a synthetic root namespace; use fetchOnly.
	conn := fetchOnly(t, c, cfg, connector.Namespace{})
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Box(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case strings.HasSuffix(r.URL.Path, "/folders/0/items"):
			_, _ = io.WriteString(w, `{"total_count":1,"entries":[{"type":"file","id":"f1","name":"a","etag":"1","modified_at":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/files/f1"):
			_, _ = io.WriteString(w, `{"name":"a","size":2,"created_at":"2024-01-01T00:00:00Z","modified_at":"2024-01-01T00:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/files/f1/content"):
			_, _ = io.WriteString(w, "hi")
		case r.URL.Path == "/events":
			_, _ = io.WriteString(w, `{"entries":[{"event_type":"ITEM_UPLOAD","source":{"type":"file","id":"f1"}}],"next_stream_position":42}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := box.New(box.WithBaseURL(srv.URL), box.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn := fetchOnly(t, c, cfg, connector.Namespace{})
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, "0"); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Notion(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case "/search":
			_, _ = io.WriteString(w, `{"results":[{"id":"p1","last_edited_time":"2024-01-01T00:00:00Z"}],"has_more":false,"next_cursor":""}`)
		case "/pages/p1":
			_, _ = io.WriteString(w, `{"id":"p1","created_time":"2024-01-01T00:00:00Z","last_edited_time":"2024-01-01T00:00:00Z"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := notion.New(notion.WithBaseURL(srv.URL), notion.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn := fetchOnly(t, c, cfg, connector.Namespace{})
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Confluence(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/wiki/api/v2/spaces"):
			_, _ = io.WriteString(w, `{"results":[{"id":"sp1","key":"K","name":"Eng"}]}`)
		case strings.HasPrefix(r.URL.Path, "/wiki/api/v2/pages/p1"):
			_, _ = io.WriteString(w, `{"title":"hello","createdAt":"2024-01-01T00:00:00Z","version":{"createdAt":"2024-01-02T00:00:00Z"}}`)
		case strings.HasPrefix(r.URL.Path, "/wiki/api/v2/pages"):
			_, _ = io.WriteString(w, `{"results":[{"id":"p1","title":"hello","version":{"number":2,"createdAt":"2024-01-02T00:00:00Z"}}]}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := confluence.New(confluence.WithBaseURL(srv.URL), confluence.WithHTTPClient(srv.Client()))
	creds := []byte(fmt.Sprintf(`{"email":"u@x.com","api_token":"tok","site_url":%q}`, srv.URL))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "sp1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Jira(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			_, _ = io.WriteString(w, `{"accountId":"u"}`)
		case r.URL.Path == "/rest/api/3/project":
			_, _ = io.WriteString(w, `[{"id":"1","key":"ENG","name":"Engineering"}]`)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			_, _ = io.WriteString(w, `{"startAt":0,"maxResults":100,"total":1,"issues":[{"key":"ENG-1","fields":{"updated":"2024-01-02T00:00:00.000+0000"}}]}`)
		case r.URL.Path == "/rest/api/3/issue/ENG-1":
			_, _ = io.WriteString(w, `{"key":"ENG-1","fields":{"summary":"hello","created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-02T00:00:00.000+0000"}}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := jira.New(jira.WithBaseURL(srv.URL), jira.WithHTTPClient(srv.Client()))
	creds := []byte(fmt.Sprintf(`{"email":"u@x.com","api_token":"tok","site_url":%q}`, srv.URL))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ENG"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"webhookEvent":"jira:issue_updated","issue":{"key":"ENG-1"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_GitHub(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"u"}`)
		case r.URL.Path == "/user/repos":
			_, _ = io.WriteString(w, `[{"full_name":"u/repo","name":"repo","private":false}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/u/repo/issues/1"):
			_, _ = io.WriteString(w, `{"title":"hi","user":{"login":"u"},"created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z","body":"x"}`)
		case r.URL.Path == "/repos/u/repo/issues":
			_, _ = io.WriteString(w, `[{"number":1,"updated_at":"2024-01-02T00:00:00Z"}]`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gh.New(gh.WithBaseURL(srv.URL), gh.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "u/repo"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"u/r"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_GitLab(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"id":1,"username":"u"}`)
		case r.URL.Path == "/projects":
			_, _ = io.WriteString(w, `[{"id":7,"path_with_namespace":"u/r","name":"r"}]`)
		case strings.HasPrefix(r.URL.Path, "/projects/7/issues/1"):
			_, _ = io.WriteString(w, `{"title":"hi","author":{"username":"u"},"created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"}`)
		case r.URL.Path == "/projects/7/issues":
			_, _ = io.WriteString(w, `[{"iid":1,"updated_at":"2024-01-02T00:00:00Z"}]`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gitlab.New(gitlab.WithBaseURL(srv.URL), gitlab.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "7"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"object_kind":"issue","object_attributes":{"iid":42,"action":"open"},"project":{"id":7}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_Teams(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case "/me/joinedTeams":
			_, _ = io.WriteString(w, `{"value":[{"id":"team1","displayName":"Eng"}]}`)
		case "/teams/team1/channels":
			_, _ = io.WriteString(w, `{"value":[{"id":"ch1","displayName":"general"}]}`)
		case "/teams/team1/channels/ch1/messages":
			_, _ = io.WriteString(w, `{"value":[{"id":"m1","etag":"e","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case "/teams/team1/channels/ch1/messages/m1":
			_, _ = io.WriteString(w, `{"subject":"hello","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","from":{"user":{"displayName":"Alice"}}}`)
		case "/teams/team1/channels/ch1/messages/delta":
			_, _ = io.WriteString(w, `{"value":[{"id":"m1","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],"@odata.deltaLink":"NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	})
	mux.Handle("/", handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := teams.New(teams.WithBaseURL(srv.URL), teams.WithHTTPClient(srv.Client()))
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`)}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "team1/ch1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"value":[{"changeType":"created","resource":"teams('team1')/channels('ch1')/messages('m1')"}]}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_KChat(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/channels.list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"channels":[{"id":"C1","name":"general"}]}`)
	})
	mux.HandleFunc("/channels.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"messages":[{"id":"M1","ts":1700000000,"user":"U1","text":"hi"}]}`)
	})
	mux.HandleFunc("/messages.get", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","ts":1700000000,"user":"U1","text":"hi"}`)
	})
	mux.HandleFunc("/channels.changes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"changes":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := kchat.New(kchat.WithBaseURL(srv.URL), kchat.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(kchat.Credentials{APIToken: "kc-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"type":"message.created","channel":"C1","message":{"id":"M1","updated_at":"2024-01-01T00:00:00Z"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
}

func TestConnectorSmoke_S3(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>bucket</Name>
<KeyCount>1</KeyCount>
<IsTruncated>false</IsTruncated>
<Contents><Key>doc1.txt</Key><ETag>"e"</ETag><Size>5</Size><LastModified>2024-01-01T00:00:00.000Z</LastModified></Contents>
</ListBucketResult>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := s3.New(s3.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(s3.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", Endpoint: srv.URL, Bucket: "bucket", Region: "us-east-1"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "bucket"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Linear(t *testing.T) {
	t.Parallel()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		s := string(body)
		switch {
		case strings.Contains(s, "viewer"):
			_, _ = io.WriteString(w, `{"data":{"viewer":{"id":"u1"}}}`)
		case strings.Contains(s, "teams {"):
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"T1","key":"K","name":"Team"}]}}}`)
		case strings.Contains(s, "team(id:"), strings.Contains(s, "$teamId"):
			_, _ = io.WriteString(w, `{"data":{"team":{"issues":{"nodes":[{"id":"I1","identifier":"ENG-1","title":"x","updatedAt":"2024-01-01T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}}}}`)
		case strings.Contains(s, "issue(id:"), strings.Contains(s, "$id"):
			_, _ = io.WriteString(w, `{"data":{"issue":{"id":"I1","identifier":"ENG-1","title":"x","description":"y","updatedAt":"2024-01-01T00:00:00Z","createdAt":"2024-01-01T00:00:00Z"}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(linear.Credentials{APIKey: "lin_api_test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"action":"create","type":"Issue","data":{"id":"I2","identifier":"ENG-2","updatedAt":"2024-01-02T00:00:00Z"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) == 0 {
		t.Fatalf("expected at least one change")
	}
	_ = calls
}

func TestConnectorSmoke_Asana(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"gid":"U1"}}`)
	})
	mux.HandleFunc("/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"gid":"W1","name":"Acme"}]}`)
	})
	mux.HandleFunc("/projects", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"gid":"P1","name":"Backend"}]}`)
	})
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"gid":"T1","name":"a","modified_at":"2024-01-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/tasks/T1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"gid":"T1","name":"a","notes":"detail","created_at":"2024-01-01T00:00:00Z","modified_at":"2024-01-01T00:00:00Z"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(asana.Credentials{AccessToken: "as-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "P1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Discord(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/users/@me/guilds", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"G1","name":"Guild"}]`)
	})
	mux.HandleFunc("/guilds/G1/channels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"C1","name":"general","type":0}]`)
	})
	mux.HandleFunc("/channels/C1/messages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"M1","timestamp":"2024-01-01T00:00:00Z"}]`)
	})
	mux.HandleFunc("/channels/C1/messages/M1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","content":"hi","timestamp":"2024-01-01T00:00:00Z","author":{"username":"alice"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(discord.Credentials{BotToken: "bot-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_Salesforce(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"done":true,"records":[{"Id":"a","SystemModstamp":"2024-01-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/services/data/v59.0/sobjects/Account/a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Id":"a","Name":"Acme","SystemModstamp":"2024-01-01T00:00:00Z","CreatedDate":"2024-01-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL), salesforce.WithObjects([]string{"Account"}))
	creds, _ := json.Marshal(salesforce.Credentials{InstanceURL: srv.URL, AccessToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_HubSpot(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/crm/v3/objects/contacts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"c1","updatedAt":"2024-01-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/crm/v3/objects/contacts/c1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"c1","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-01T00:00:00Z","properties":{"name":"Alice"}}`)
	})
	mux.HandleFunc("/crm/v3/objects/contacts/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"c1","updatedAt":"2024-01-01T00:00:00Z","properties":{"hs_lastmodifieddate":"2024-01-01T00:00:00Z"}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()), hubspot.WithObjects([]string{"contacts"}))
	creds, _ := json.Marshal(hubspot.Credentials{AccessToken: "pat-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "contacts"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}

func TestConnectorSmoke_GoogleSharedDrives(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/drives", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"drives":[{"id":"D1","name":"Engineering"}]}`)
	})
	mux.HandleFunc("/files", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"files":[{"id":"f1","name":"a","modifiedTime":"2024-01-01T00:00:00Z","version":"1"}]}`)
	})
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = io.WriteString(w, "hi")

			return
		}
		_, _ = io.WriteString(w, `{"id":"f1","name":"a","mimeType":"text/plain","size":"2","modifiedTime":"2024-01-01T00:00:00Z","createdTime":"2024-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("/changes/startPageToken", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"startPageToken":"t1"}`)
	})
	mux.HandleFunc("/changes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"changes":[],"newStartPageToken":"t2"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googledrive.NewSharedDrives(googledrive.WithBaseURL(srv.URL), googledrive.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(googledrive.Credentials{AccessToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, _ := runListNamespacesAndFetch(t, c, cfg)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "D1"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
}
