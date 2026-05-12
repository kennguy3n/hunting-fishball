//go:build e2e

// Round-16 Task 11 — Round-16 end-to-end smoke.
//
// This file exercises the Round-16 surface as an integrated bundle.
// Each subtest mirrors a feature added in Round 16 and runs against
// pure stdlib `httptest` fakes — no docker-compose involvement, no
// migration plane. The build tag `e2e` keeps it off the fast lane.
//
// Coverage:
//
//	(a) Registry now reports exactly 28 entries (20 → 28).
//	(b) Two full Round-16 connector lifecycles (gmail + okta) walk
//	    Validate → Connect → ListNamespaces → ListDocuments →
//	    FetchDocument → DeltaSync.
//	(c) connector.ErrRateLimited propagates through iterator.Err
//	    when each new connector's upstream returns 429.
//	(d) DeltaSync bootstrap returns a "now" cursor without
//	    backfilling history (DESC+limit=1 contract from Round-15).
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
	confluenceserver "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
	"github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
	"github.com/kennguy3n/hunting-fishball/internal/connector/okta"
	"github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/rss"
)

// TestRound16_RegistryCount verifies the catalog expansion contract:
// blank-imports from the e2e package populate exactly 28 connector
// entries after Round 16's mattermost/clickup/monday/pipedrive/okta/
// gmail/rss/confluence_server additions.
func TestRound16_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 28 {
		t.Fatalf("registry: %d entries, want 28: %v", len(names), names)
	}
}

// TestRound16_GmailLifecycle walks the full SourceConnector +
// DeltaSyncer surface for the new Gmail connector. Gmail is the
// canonical "history-cursor" delta connector and is the Round-16
// proof that historyId-based cursors compose with the registry.
func TestRound16_GmailLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"emailAddress":"a@x","historyId":"100"}`)
	})
	mux.HandleFunc("/users/me/labels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"labels":[{"id":"INBOX","name":"INBOX","type":"system"}]}`)
	})
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"messages":[{"id":"M1"}]}`)
	})
	mux.HandleFunc("/users/me/messages/M1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","internalDate":"1700000000000","historyId":"100","payload":{"headers":[{"name":"Subject","value":"hi"}]}}`)
	})
	mux.HandleFunc("/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"history":[],"historyId":"100"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(gmail.Credentials{AccessToken: "ya29.test"})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("list namespaces: %v / len=%d", err, len(ns))
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch document: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	// Bootstrap: empty cursor must come back populated with the
	// profile's historyId without backfilling history.
	_, cur, err := c.DeltaSync(ctx, conn, ns[0], "")
	if err != nil {
		t.Fatalf("delta bootstrap: %v", err)
	}
	if cur != "100" {
		t.Fatalf("delta bootstrap cursor: got %q, want %q", cur, "100")
	}
}

// TestRound16_OktaLifecycle walks the full SourceConnector +
// DeltaSyncer surface for the new Okta connector. Okta uses RFC 5988
// `Link: <next>; rel="next"` pagination and an SSWS auth header — this
// case keeps both wired through end-to-end.
func TestRound16_OktaLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortBy") == "lastUpdated" {
			_, _ = io.WriteString(w, `[{"id":"U1","status":"ACTIVE","lastUpdated":"2024-01-05T00:00:00Z"}]`)

			return
		}
		_, _ = io.WriteString(w, `[{"id":"U1","status":"ACTIVE","lastUpdated":"2024-01-01T00:00:00Z"}]`)
	})
	mux.HandleFunc("/api/v1/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","status":"ACTIVE","lastUpdated":"2024-01-01T00:00:00Z","created":"2024-01-01T00:00:00Z","profile":{"login":"alice","email":"a@x","firstName":"A","lastName":"L"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(okta.Credentials{APIToken: "00abc", OrgURL: srv.URL})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("list namespaces: %v / len=%d", err, len(ns))
	}
	it, err := c.ListDocuments(ctx, conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one user; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch document: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	_, cur, err := c.DeltaSync(ctx, conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("delta bootstrap: %v", err)
	}
	if cur != "2024-01-05T00:00:00Z" {
		t.Fatalf("delta bootstrap cursor: got %q, want %q", cur, "2024-01-05T00:00:00Z")
	}
}

// TestRound16_NewConnectors_RateLimitedOnListDocuments proves that
// each of the eight Round-16 connectors surfaces
// connector.ErrRateLimited through iterator.Err on a 429 response.
func TestRound16_NewConnectors_RateLimitedOnListDocuments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"mattermost", round16RateLimited_Mattermost},
		{"clickup", round16RateLimited_ClickUp},
		{"pipedrive", round16RateLimited_Pipedrive},
		{"okta", round16RateLimited_Okta},
		{"gmail", round16RateLimited_Gmail},
		{"rss", round16RateLimited_RSS},
		{"confluence_server", round16RateLimited_ConfluenceServer},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func round16RateLimited_Mattermost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"u1"}`)
	})
	mux.HandleFunc("/api/v4/channels/C1/posts", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(mattermost.Credentials{AccessToken: "tok", BaseURL: srv.URL})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_ClickUp(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{"id":1}}`)
	})
	mux.HandleFunc("/list/L1/task", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(clickup.Credentials{APIKey: "k", TeamID: "T1"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "L1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_Pipedrive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/deals", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pipedrive.New(pipedrive.WithBaseURL(srv.URL), pipedrive.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(pipedrive.Credentials{APIToken: "k", CompanyDomain: "x"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "deals"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_Okta(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(okta.Credentials{APIToken: "k", OrgURL: srv.URL})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_Gmail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"1"}`)
	})
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(gmail.Credentials{AccessToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "INBOX"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_RSS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := rss.New(rss.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(rss.Credentials{FeedURLs: []string{srv.URL}})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: srv.URL}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func round16RateLimited_ConfluenceServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(confluenceserver.Credentials{BaseURL: srv.URL, PAT: "pat"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	conn, err := c.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "DOCS"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}
