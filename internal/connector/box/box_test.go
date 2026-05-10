package box_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/box"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *box.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := box.New(box.WithHTTPClient(srv.Client()), box.WithBaseURL(srv.URL))
	return srv, c
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := box.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case strings.HasSuffix(r.URL.Path, "/folders/0/items"):
			_, _ = io.WriteString(w, `{"total_count":1,"entries":[{"type":"file","id":"f1","name":"a","etag":"1","modified_at":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/files/f1"):
			_, _ = io.WriteString(w, `{"name":"a","size":2,"created_at":"2024-01-01T00:00:00Z","modified_at":"2024-01-01T00:00:00Z"}`)
		case strings.HasSuffix(r.URL.Path, "/files/f1/content"):
			_, _ = io.WriteString(w, "hi")
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
	it, err := c.ListDocuments(context.Background(), conn, connector.Namespace{}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	doc, err := c.FetchDocument(context.Background(), conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hi" {
		t.Fatalf("body: %q", body)
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case "/events":
			_, _ = io.WriteString(w, `{"entries":[{"event_type":"ITEM_UPLOAD","source":{"type":"file","id":"f1"}},{"event_type":"ITEM_TRASH","source":{"type":"file","id":"f2"}}],"next_stream_position":42}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`)})
	ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, "0")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if ch[0].Kind != connector.ChangeUpserted || ch[1].Kind != connector.ChangeDeleted {
		t.Fatalf("kinds: %+v", ch)
	}
	if cur != "42" {
		t.Fatalf("cur: %q", cur)
	}
}
