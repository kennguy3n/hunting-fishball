package coda_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/coda"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(coda.Credentials{AccessToken: "tok"})

	return b
}

func TestCoda_Validate(t *testing.T) {
	t.Parallel()
	c := coda.New()
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

func TestCoda_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestCoda_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := coda.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 1 || ns[0].ID != "docs" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestCoda_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"alice"}`)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pageToken") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"id":"D1"}],"nextPageToken":"p2"}`)
		case "p2":
			_, _ = io.WriteString(w, `{"items":[{"id":"D2"}]}`)
		default:
			_, _ = io.WriteString(w, `{"items":[]}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "docs"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "D1" || ids[1] != "D2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestCoda_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "docs"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestCoda_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/docs/D1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"D1","name":"Wiki","owner":"alice@x","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-06-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "docs", ID: "D1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Wiki" || doc.Author != "alice@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Wiki") {
		t.Fatalf("body=%q", body)
	}
}

func TestCoda_DeltaSync_BootstrapReturnsLatestUpdatedAt(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			_, _ = io.WriteString(w, `{"items":[{"id":"D9","updatedAt":"2024-06-30T12:00:00Z"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "docs"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur != "2024-06-30T12:00:00Z" {
		t.Fatalf("cursor=%q", cur)
	}
}

func TestCoda_DeltaSync_StopsAtCursor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[
			{"id":"D2","updatedAt":"2024-06-02T00:00:00Z"},
			{"id":"D1","updatedAt":"2024-06-01T00:00:00Z"}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "docs"}, "2024-06-01T12:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "D2" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestCoda_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/docs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := coda.New(coda.WithBaseURL(srv.URL), coda.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "docs"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestCoda_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := coda.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
