package dropbox_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/dropbox"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *dropbox.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	c := dropbox.New(
		dropbox.WithHTTPClient(srv.Client()),
		dropbox.WithBaseURL(srv.URL),
		dropbox.WithContentURL(srv.URL),
	)
	return srv, c
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := dropbox.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/get_current_account":
			_, _ = io.WriteString(w, `{"account_id":"u"}`)
		case "/files/list_folder":
			_, _ = io.WriteString(w, `{"entries":[{".tag":"file","id":"id:1","name":"a","rev":"r","server_modified":"2024-01-01T00:00:00Z"}],"cursor":"CUR","has_more":false}`)
		case "/files/download":
			w.Header().Set("Dropbox-API-Result", `{"name":"a","size":2,"server_modified":"2024-01-01T00:00:00Z"}`)
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
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/get_current_account"):
			_, _ = io.WriteString(w, `{"account_id":"u"}`)
		case r.URL.Path == "/files/list_folder/get_latest_cursor":
			_, _ = io.WriteString(w, `{"cursor":"CUR0"}`)
		case r.URL.Path == "/files/list_folder/continue":
			_, _ = io.WriteString(w, `{"entries":[{".tag":"file","id":"id:1"},{".tag":"deleted","id":"id:2"}],"cursor":"CUR1"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`),
	})

	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur != "CUR0" {
		t.Fatalf("cur: %q", cur)
	}

	ch, cur2, err := c.DeltaSync(context.Background(), conn, connector.Namespace{}, "CUR0")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if cur2 != "CUR1" {
		t.Fatalf("cur2: %q", cur2)
	}
}
