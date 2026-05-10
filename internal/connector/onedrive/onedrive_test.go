package onedrive_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/onedrive"
)

func newServer(h http.Handler) (*httptest.Server, *onedrive.Connector) {
	srv := httptest.NewServer(h)
	return srv, onedrive.New(onedrive.WithHTTPClient(srv.Client()), onedrive.WithBaseURL(srv.URL))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := onedrive.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("expected invalid config, got %v", err)
	}
}

func TestConnectListAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/root/children"):
			_, _ = io.WriteString(w, `{"value":[{"id":"f1","name":"a","eTag":"e","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/items/f1"):
			_, _ = io.WriteString(w, `{"name":"a","size":1,"file":{"mimeType":"text/plain"}}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/items/f1/content"):
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
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 {
		t.Fatalf("ns: %+v, err=%v", ns, err)
	}
	it, err := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
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
	srv, c := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/me/drive/root/delta"):
			_, _ = io.WriteString(w, `{"value":[{"id":"a","lastModifiedDateTime":"2024-01-01T00:00:00Z"},{"id":"b","deleted":{}}],"@odata.deltaLink":"NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`)})
	ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "me"}, "")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if cur != "NEXT" {
		t.Fatalf("cursor: %q", cur)
	}
}
