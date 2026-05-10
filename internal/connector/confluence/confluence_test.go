package confluence_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/confluence"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *confluence.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	return srv, confluence.New(confluence.WithHTTPClient(srv.Client()), confluence.WithBaseURL(srv.URL))
}

func creds(site string) []byte {
	return []byte(fmt.Sprintf(`{"email":"u@x.com","api_token":"tok","site_url":%q}`, site))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := confluence.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListNamespacesAndDocs(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: creds(srv.URL),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 {
		t.Fatalf("ns: %+v err=%v", ns, err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	if it.Doc().ID != "p1" {
		t.Fatalf("ref: %+v", it.Doc())
	}
	doc, err := c.FetchDocument(context.Background(), conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "hello" {
		t.Fatalf("title: %q", doc.Title)
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/wiki/api/v2/spaces"):
			_, _ = io.WriteString(w, `{"results":[]}`)
		case strings.HasPrefix(r.URL.Path, "/wiki/api/v2/pages"):
			_, _ = io.WriteString(w, `{"results":[{"id":"p1","status":"current","version":{"createdAt":"2025-01-01T00:00:00Z"}},{"id":"p2","status":"trashed","version":{"createdAt":"2025-01-02T00:00:00Z"}}]}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: creds(srv.URL),
	})
	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "sp1"}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur == "" {
		t.Fatalf("empty cur")
	}
	ch, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "sp1"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if ch[1].Kind != connector.ChangeDeleted {
		t.Fatalf("kinds: %+v", ch)
	}
}
