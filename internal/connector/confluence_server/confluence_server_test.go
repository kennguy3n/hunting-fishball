package confluenceserver_test

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
	confluenceserver "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
)

func validCreds(t *testing.T, base string) []byte {
	t.Helper()
	b, _ := json.Marshal(confluenceserver.Credentials{BaseURL: base, PAT: "pat"})

	return b
}

func TestConfluenceServer_Validate(t *testing.T) {
	t.Parallel()
	s := confluenceserver.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy pat", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, "https://wiki")}, true},
		{"happy basic", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_url":"https://x","username":"a","password":"b"}`)}, true},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing base_url", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"pat":"x"}`)}, false},
		{"missing auth", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_url":"https://x"}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := s.Validate(context.Background(), tc.cfg)
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

func TestConfluenceServer_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	_, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestConfluenceServer_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/space", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"key":"DOCS","name":"Docs"}],"size":1,"limit":50}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	ns, err := s.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].ID != "DOCS" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestConfluenceServer_ListNamespaces_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/space", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, err := s.ListNamespaces(context.Background(), conn)
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestConfluenceServer_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "DOCS"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestConfluenceServer_ListDocuments_HappyPath(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"id":"1","title":"P1","version":{"when":"2024-01-01T00:00:00Z"}}],"size":1,"limit":50}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "DOCS"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	count := 0
	for it.Next(context.Background()) {
		count++
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) || count != 1 {
		t.Fatalf("count=%d err=%v", count, it.Err())
	}
}

func TestConfluenceServer_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"1","title":"P1","version":{"when":"2024-01-01T00:00:00Z","by":{"displayName":"alice"}},"body":{"storage":{"value":"<p>hi</p>"}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := s.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "DOCS", ID: "1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "P1" || doc.Author != "alice" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hi") {
		t.Fatalf("body=%q", body)
	}
}

func TestConfluenceServer_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var gotCQL string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content/search", func(w http.ResponseWriter, r *http.Request) {
		cql := r.URL.Query().Get("cql")
		if strings.Contains(cql, "ORDER BY lastModified DESC") {
			_, _ = io.WriteString(w, `{"results":[{"version":{"when":"2024-01-05T00:00:00Z"}}]}`)

			return
		}
		gotCQL = cql
		_, _ = io.WriteString(w, `{"results":[{"id":"99","version":{"when":"2024-01-10T00:00:00Z"}}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "DOCS"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-05T00:00:00Z" {
		t.Fatalf("initial cur=%q changes=%v", cur, changes)
	}
	changes, newCur, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "DOCS"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if !strings.Contains(gotCQL, "lastModified >") {
		t.Fatalf("cql=%q", gotCQL)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "99" || newCur != "2024-01-10T00:00:00Z" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

// TestConfluenceServer_DeltaSync_CursorPreservesSecondsPrecision regresses
// against a Round-16 review finding: the CQL cursor must preserve seconds
// precision, otherwise pages modified within the cursor's minute are
// re-emitted as duplicate ChangeUpserted events on every subsequent poll.
func TestConfluenceServer_DeltaSync_CursorPreservesSecondsPrecision(t *testing.T) {
	t.Parallel()
	var gotCQL string
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content/search", func(w http.ResponseWriter, r *http.Request) {
		gotCQL = r.URL.Query().Get("cql")
		_, _ = io.WriteString(w, `{"results":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	// Bootstrap cursor stored in RFC3339 with full second precision.
	if _, _, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "DOCS"}, "2024-01-05T12:30:45Z"); err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if !strings.Contains(gotCQL, `lastModified > "2024-01-05 12:30:45"`) {
		t.Fatalf("cql lost seconds precision: %q", gotCQL)
	}
}

func TestConfluenceServer_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/user/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/rest/api/content/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := confluenceserver.New(confluenceserver.WithBaseURL(srv.URL), confluenceserver.WithHTTPClient(srv.Client()))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, _, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "DOCS"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "DOCS"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestConfluenceServer_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	s := confluenceserver.New()
	_, err := s.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
