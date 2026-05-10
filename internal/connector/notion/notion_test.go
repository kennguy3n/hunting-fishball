package notion_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/notion"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *notion.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := notion.New(notion.WithHTTPClient(srv.Client()), notion.WithBaseURL(srv.URL))
	return srv, c
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := notion.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{}, connector.ListOpts{})
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	doc, err := c.FetchDocument(context.Background(), conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.MIMEType != "application/json" {
		t.Fatalf("mime: %q", doc.MIMEType)
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/me" {
			_, _ = io.WriteString(w, `{"id":"u"}`)
			return
		}
		if r.URL.Path == "/search" {
			_, _ = io.WriteString(w, `{"results":[{"id":"p1","last_edited_time":"2025-01-01T00:00:00Z"},{"id":"p2","archived":true,"last_edited_time":"2025-01-02T00:00:00Z"}]}`)
			return
		}
		http.Error(w, r.URL.String(), http.StatusNotFound)
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`),
	})

	// Empty cursor returns "now" stamp without changes.
	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur == "" {
		t.Fatalf("empty cursor")
	}

	cutoff := "2024-01-01T00:00:00Z"
	ch, cur2, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, cutoff)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("want 2 changes, got %d: %+v", len(ch), ch)
	}
	if ch[0].Kind != connector.ChangeUpserted || ch[1].Kind != connector.ChangeDeleted {
		t.Fatalf("kinds: %+v", ch)
	}
	high, _ := time.Parse(time.RFC3339, cur2)
	if !high.After(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("high: %s", cur2)
	}
}
