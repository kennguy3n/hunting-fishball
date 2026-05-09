package googledrive_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
)

func newDriveServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv
}

func newDriveConnector(srv *httptest.Server) *googledrive.Connector {
	return googledrive.New(
		googledrive.WithBaseURL(srv.URL),
		googledrive.WithHTTPClient(srv.Client()),
	)
}

func validCreds() []byte {
	b, _ := json.Marshal(googledrive.Credentials{AccessToken: "tok"})

	return b
}

func TestGoogleDrive_Validate(t *testing.T) {
	t.Parallel()

	g := googledrive.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()}, true},
		{"missing tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds()}, false},
		{"missing source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds()}, false},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("not json")}, false},
		{"missing access token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := g.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected error")
			}
			if !tc.ok && !errors.Is(err, connector.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig: %v", err)
			}
		})
	}
}

func TestGoogleDrive_Connect_AuthCheck(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"user":{"emailAddress":"a@b"}}`))
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	_, err := g.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: validCreds(),
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestGoogleDrive_ListNamespaces(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/drives", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"drives":[{"id":"D1","name":"Engineering"},{"id":"D2","name":"Ops"}]}`))
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	conn, err := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := g.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 3 {
		t.Fatalf("namespaces: %d", len(ns))
	}
	if ns[0].Kind != "my_drive" {
		t.Fatalf("first ns kind: %q", ns[0].Kind)
	}
}

func TestGoogleDrive_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		page := r.URL.Query().Get("pageToken")
		switch {
		case page == "" && n == 1:
			_, _ = w.Write([]byte(`{"nextPageToken":"p2","files":[{"id":"f1","name":"a","modifiedTime":"2025-01-01T00:00:00Z","version":"1"}]}`))
		case page == "p2":
			_, _ = w.Write([]byte(`{"files":[{"id":"f2","name":"b","modifiedTime":"2025-01-02T00:00:00Z","version":"1"}]}`))
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
	it, err := g.ListDocuments(context.Background(), conn, connector.Namespace{ID: "my-drive"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()

	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "f1" || ids[1] != "f2" {
		t.Fatalf("ids: %+v", ids)
	}
}

// TestGoogleDrive_ListDocuments_PageSizeRespectsCaller verifies that
// the connector forwards the caller's PageSize to Drive verbatim
// instead of clamping it upward. Regression coverage for the original
// maxInt(opts.PageSize, 100) bug, which would silently turn a
// caller-supplied 10 into 100.
func TestGoogleDrive_ListDocuments_PageSizeRespectsCaller(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		size int
		want string
	}{
		{"caller below default", 10, "10"},
		{"caller above default", 250, "250"},
		{"zero falls back to default", 0, "100"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got atomic.Value
			mux := http.NewServeMux()
			mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
			mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
				got.Store(r.URL.Query().Get("pageSize"))
				_, _ = w.Write([]byte(`{"files":[]}`))
			})
			srv := newDriveServer(t, mux)
			g := newDriveConnector(srv)
			conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
			it, err := g.ListDocuments(context.Background(), conn, connector.Namespace{ID: "my-drive"}, connector.ListOpts{PageSize: tc.size})
			if err != nil {
				t.Fatalf("ListDocuments: %v", err)
			}
			defer func() { _ = it.Close() }()
			for it.Next(context.Background()) {
			}
			if g, _ := got.Load().(string); g != tc.want {
				t.Fatalf("pageSize: got %q want %q", g, tc.want)
			}
		})
	}
}

func TestGoogleDrive_FetchDocument(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write([]byte("hello drive"))

			return
		}
		_, _ = w.Write([]byte(`{
			"id":"f1","name":"hello.txt","mimeType":"text/plain","size":"11",
			"modifiedTime":"2025-01-01T00:00:00Z",
			"createdTime":"2024-12-01T00:00:00Z",
			"owners":[{"emailAddress":"a@b","displayName":"A"}]
		}`))
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
	doc, err := g.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "my-drive", ID: "f1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello drive" {
		t.Fatalf("body: %q", body)
	}
	if doc.MIMEType != "text/plain" {
		t.Fatalf("mime: %q", doc.MIMEType)
	}
	if doc.Title != "hello.txt" {
		t.Fatalf("title: %q", doc.Title)
	}
	if doc.Author != "a@b" {
		t.Fatalf("author: %q", doc.Author)
	}
}

func TestGoogleDrive_DeltaSync(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/changes/startPageToken", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"startPageToken":"start-1"}`))
	})
	mux.HandleFunc("/changes", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page := r.URL.Query().Get("pageToken")
		if page != "start-1" {
			http.Error(w, "bad token", http.StatusBadRequest)

			return
		}
		_, _ = w.Write([]byte(`{
			"newStartPageToken":"start-2",
			"changes":[
				{"fileId":"f1","file":{"id":"f1","modifiedTime":"2025-01-01T00:00:00Z"}},
				{"fileId":"f2","removed":true}
			]
		}`))
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})

	// First call with empty cursor returns the start token.
	changes, cursor, err := g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "my-drive"}, "")
	if err != nil {
		t.Fatalf("DeltaSync 1: %v", err)
	}
	if len(changes) != 0 || cursor != "start-1" {
		t.Fatalf("first call: changes=%d cursor=%q", len(changes), cursor)
	}

	// Subsequent call returns the changes and next cursor.
	changes, cursor, err = g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "my-drive"}, "start-1")
	if err != nil {
		t.Fatalf("DeltaSync 2: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes: %d", len(changes))
	}
	if changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("expected delete change, got %v", changes[1].Kind)
	}
	if cursor != "start-2" {
		t.Fatalf("cursor: %q", cursor)
	}
}

func TestGoogleDrive_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	g := googledrive.New()
	if _, err := g.Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestGoogleDrive_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector("google_drive"); err != nil {
		t.Fatalf("connector not registered: %v", err)
	}
}

// Validates that the URL.PathEscape path used in FetchDocument doesn't
// accidentally double-escape when ref.ID has special characters.
func TestGoogleDrive_FetchDocument_EscapesID(t *testing.T) {
	t.Parallel()

	var seen string
	mux := http.NewServeMux()
	mux.HandleFunc("/about", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		seen = r.RequestURI
		if r.URL.Query().Get("alt") == "media" {
			_, _ = w.Write([]byte("body"))

			return
		}
		_, _ = w.Write([]byte(`{"id":"x","name":"x","mimeType":"text/plain","modifiedTime":"2025-01-01T00:00:00Z"}`))
	})
	srv := newDriveServer(t, mux)
	g := newDriveConnector(srv)

	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds()})
	id := "abc/def?weird"
	doc, err := g.FetchDocument(context.Background(), conn, connector.DocumentRef{ID: id})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()

	want := "/files/" + url.PathEscape(id)
	if !strings.HasPrefix(seen, want) {
		t.Fatalf("RequestURI: %q want prefix %q", seen, want)
	}
}
