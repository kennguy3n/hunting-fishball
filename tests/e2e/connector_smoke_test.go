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
//   - The process-global connector registry has exactly 12 entries
//     after blank-imports complete.
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
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/box"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	_ "github.com/kennguy3n/hunting-fishball/internal/connector/teams"

	// Direct imports so each smoke case can call the connector's
	// `New(With…)` option helpers without going through the registry's
	// stripped-down factory.
	"github.com/kennguy3n/hunting-fishball/internal/connector/box"
	"github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
	"github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
	gh "github.com/kennguy3n/hunting-fishball/internal/connector/github"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/jira"
	"github.com/kennguy3n/hunting-fishball/internal/connector/notion"
	"github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	"github.com/kennguy3n/hunting-fishball/internal/connector/teams"
)

// expectedConnectors is the canonical list of registered connector
// names. The smoke suite is the on-call's check that no connector
// silently disappears from a binary's blank-import list.
var expectedConnectors = []string{
	"box",
	"confluence",
	"dropbox",
	"github",
	"gitlab",
	"google_drive",
	"jira",
	"notion",
	"onedrive",
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
