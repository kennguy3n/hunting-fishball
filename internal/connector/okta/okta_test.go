package okta_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/okta"
)

func validCreds(t *testing.T, base string) []byte {
	t.Helper()
	b, _ := json.Marshal(okta.Credentials{APIToken: "00abc", OrgURL: base})

	return b
}

func TestOkta_Validate(t *testing.T) {
	t.Parallel()
	o := okta.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, "https://acme.okta.com")}, true},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"org_url":"https://x"}`)}, false},
		{"missing url", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_token":"00abc"}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := o.Validate(context.Background(), tc.cfg)
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

func TestOkta_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	_, err := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestOkta_ListNamespaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	ns, err := o.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 2 || ns[0].ID != "users" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestOkta_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	var nextURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "U1" {
			_, _ = io.WriteString(w, `[{"id":"U2","lastUpdated":"2024-01-02T00:00:00Z"}]`)

			return
		}
		w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
		_, _ = io.WriteString(w, `[{"id":"U1","lastUpdated":"2024-01-01T00:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	nextURL = srv.URL + "/api/v1/users?after=U1"
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := o.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
}

func TestOkta_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := o.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestOkta_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","created":"2024-01-01T00:00:00Z","lastUpdated":"2024-01-02T00:00:00Z","profile":{"login":"alice@acme.com","email":"alice@acme.com","firstName":"Alice","lastName":"X"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := o.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "users", ID: "U1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "alice@acme.com" || doc.Author != "alice@acme.com" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Alice") {
		t.Fatalf("body=%q", body)
	}
}

func TestOkta_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var gotFilter string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortOrder") == "desc" {
			_, _ = io.WriteString(w, `[{"lastUpdated":"2024-01-05T00:00:00Z"}]`)

			return
		}
		gotFilter = r.URL.Query().Get("filter")
		_, _ = io.WriteString(w, `[{"id":"U2","status":"ACTIVE","lastUpdated":"2024-01-10T00:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := o.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-05T00:00:00Z" {
		t.Fatalf("initial cur=%q changes=%v", cur, changes)
	}
	changes, newCur, err := o.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if !strings.Contains(gotFilter, "lastUpdated gt") || !strings.Contains(gotFilter, cur) {
		t.Fatalf("filter=%q", gotFilter)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "U2" || newCur != "2024-01-10T00:00:00Z" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

func TestOkta_DeltaSync_DeprovisionedDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortOrder") == "desc" {
			_, _ = io.WriteString(w, `[]`)

			return
		}
		_, _ = io.WriteString(w, `[{"id":"U9","status":"DEPROVISIONED","lastUpdated":"2024-01-10T00:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, _, err := o.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestOkta_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
	conn, _ := o.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, _, err := o.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = o.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestOkta_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	o := okta.New()
	_, err := o.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
