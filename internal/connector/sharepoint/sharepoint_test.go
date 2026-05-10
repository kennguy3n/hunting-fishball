package sharepoint_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint"
)

func newServer(handler http.Handler) (*httptest.Server, *sharepoint.Connector) {
	srv := httptest.NewServer(handler)
	c := sharepoint.New(sharepoint.WithHTTPClient(srv.Client()), sharepoint.WithBaseURL(srv.URL))
	return srv, c
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := sharepoint.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		want error
	}{
		{"missing tenant", connector.ConnectorConfig{}, connector.ErrInvalidConfig},
		{"missing source", connector.ConnectorConfig{TenantID: "t"}, connector.ErrInvalidConfig},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, connector.ErrInvalidConfig},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("not json")}, connector.ErrInvalidConfig},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("{}")}, connector.ErrInvalidConfig},
		{"ok", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`)}, nil},
	}
	for _, tc := range cases {
		err := c.Validate(context.Background(), tc.cfg)
		if (tc.want == nil) != (err == nil) {
			t.Errorf("%s: err=%v want=%v", tc.name, err, tc.want)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err=%v want=%v", tc.name, err, tc.want)
		}
	}
}

func TestConnectAndListNamespaces(t *testing.T) {
	t.Parallel()
	srv, c := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case "/sites":
			_, _ = io.WriteString(w, `{"value":[{"id":"site-1","displayName":"Eng","webUrl":"https://x"}]}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "tenant", SourceID: "src", Credentials: []byte(`{"access_token":"tok"}`),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("list ns: %v", err)
	}
	if len(ns) != 1 || ns[0].ID != "site-1" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestListDocumentsAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/drive/root/children"):
			_, _ = io.WriteString(w, `{"value":[{"id":"f1","name":"a.docx","eTag":"etag1","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case strings.HasSuffix(r.URL.Path, "/drive/items/f1"):
			_, _ = io.WriteString(w, `{"id":"f1","name":"a.docx","size":42,"file":{"mimeType":"text/plain"}}`)
		case strings.HasSuffix(r.URL.Path, "/drive/items/f1/content"):
			_, _ = io.WriteString(w, "hello world")
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
	it, err := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "site1"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	ref := it.Doc()
	if ref.ID != "f1" {
		t.Fatalf("ref: %+v", ref)
	}

	doc, err := c.FetchDocument(context.Background(), conn, ref)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello world" {
		t.Fatalf("body: %q", body)
	}
}

func TestDeltaSync(t *testing.T) {
	t.Parallel()
	srv, c := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u1"}`)
		case strings.HasSuffix(r.URL.Path, "/drive/root/delta"):
			_, _ = io.WriteString(w, `{"value":[{"id":"a","lastModifiedDateTime":"2024-01-01T00:00:00Z"},{"id":"b","deleted":{}}],"@odata.deltaLink":"/x?token=NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})
	ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ns1"}, "")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if ch[0].Kind != connector.ChangeUpserted || ch[1].Kind != connector.ChangeDeleted {
		t.Fatalf("kinds: %+v", ch)
	}
	if !strings.Contains(cur, "NEXT") {
		t.Fatalf("cursor: %q", cur)
	}
}

func TestSubscribeUnsupported(t *testing.T) {
	t.Parallel()
	c := sharepoint.New()
	_, err := c.Subscribe(context.Background(), &fakeConn{}, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("want ErrNotSupported, got %v", err)
	}
}

type fakeConn struct{}

func (fakeConn) TenantID() string { return "t" }
func (fakeConn) SourceID() string { return "s" }
