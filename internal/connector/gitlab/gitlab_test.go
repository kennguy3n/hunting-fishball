package gitlab_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *gitlab.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	return srv, gitlab.New(gitlab.WithHTTPClient(srv.Client()), gitlab.WithBaseURL(srv.URL))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := gitlab.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 1 || ns[0].ID != "7" {
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
			_, _ = io.WriteString(w, `{"id":1}`)
		case strings.HasPrefix(r.URL.Path, "/projects/7/issues"):
			_, _ = io.WriteString(w, `[{"iid":1,"state":"opened","updated_at":"2025-01-02T00:00:00Z"}]`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})

	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "7"}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur == "" {
		t.Fatal("empty cur")
	}
	ch, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "7"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 1 {
		t.Fatalf("changes: %d", len(ch))
	}
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	c := gitlab.New()
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"object_kind":"issue","object_attributes":{"iid":42,"action":"open"},"project":{"id":7}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) != 1 || ch[0].Ref.ID != "42" || ch[0].Ref.NamespaceID != "7" {
		t.Fatalf("unexpected: %+v", ch)
	}
}
