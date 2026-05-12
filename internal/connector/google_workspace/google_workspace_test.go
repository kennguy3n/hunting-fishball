package googleworkspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	googleworkspace "github.com/kennguy3n/hunting-fishball/internal/connector/google_workspace"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(googleworkspace.Credentials{AccessToken: "ya29.test"})

	return b
}

func TestGoogleWorkspace_Validate(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("expected error")
				}
				if !errors.Is(err, connector.ErrInvalidConfig) {
					t.Fatalf("expected ErrInvalidConfig, got %v", err)
				}
			}
		})
	}
}

func TestGoogleWorkspace_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGoogleWorkspace_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 2 {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestGoogleWorkspace_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("maxResults") == "1" {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		if r.URL.Query().Get("pageToken") == "n1" {
			_, _ = io.WriteString(w, `{"users":[{"id":"U2","primaryEmail":"bob@x"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[{"id":"U1","primaryEmail":"alice@x"}],"nextPageToken":"n1"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "U1" || ids[1] != "U2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestGoogleWorkspace_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestGoogleWorkspace_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"users":[]}`)
	})
	mux.HandleFunc("/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","primaryEmail":"alice@acme.com","creationTime":"2024-01-01T00:00:00Z","name":{"fullName":"Alice"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "users", ID: "U1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "alice@acme.com" {
		t.Fatalf("doc.Title=%q", doc.Title)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Alice") {
		t.Fatalf("body=%q", body)
	}
}

func TestGoogleWorkspace_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortOrder") == "DESCENDING" {
			_, _ = io.WriteString(w, `{"users":[{"id":"U1","primaryEmail":"alice@x"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur == "" {
		t.Fatalf("expected non-empty bootstrap cursor")
	}
}

func TestGoogleWorkspace_DeltaSync_SuspendedMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updatedMin") == "" {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[
			{"id":"U1","primaryEmail":"alice@x","suspended":false},
			{"id":"U2","primaryEmail":"bob@x","suspended":true}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes=%+v", changes)
	}
	if changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestGoogleWorkspace_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGoogleWorkspace_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
