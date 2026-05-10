package github_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/github"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *github.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	return srv, github.New(github.WithHTTPClient(srv.Client()), github.WithBaseURL(srv.URL))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := github.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 1 {
		t.Fatalf("ns: %+v", ns)
	}
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	doc, err := c.FetchDocument(context.Background(), conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "hi" {
		t.Fatalf("title: %q", doc.Title)
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"u"}`)
		case strings.HasPrefix(r.URL.Path, "/repos/u/repo/issues"):
			_, _ = io.WriteString(w, `[{"number":1,"state":"open","updated_at":"2025-01-02T00:00:00Z"}]`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})

	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "u/repo"}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur == "" {
		t.Fatal("empty cur")
	}
	ch, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "u/repo"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 1 {
		t.Fatalf("changes: %d", len(ch))
	}
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	c := github.New()
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"action":"opened","issue":{"number":1},"repository":{"full_name":"u/r"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) != 1 || ch[0].Ref.ID != "1" || ch[0].Ref.NamespaceID != "u/r" {
		t.Fatalf("unexpected: %+v", ch)
	}
}
