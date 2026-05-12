package workday_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/workday"
)

func validCreds(t *testing.T, tenantURL string) []byte {
	t.Helper()
	b, _ := json.Marshal(workday.Credentials{AccessToken: "tok", TenantURL: tenantURL})

	return b
}

func TestWorkday_Validate(t *testing.T) {
	t.Parallel()
	c := workday.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, "https://wd5/ccx")}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"tenant_url":"https://x"}`)}, false},
		{"missing tenant_url", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"k"}`)}, false},
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

func TestWorkday_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestWorkday_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := workday.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 1 || ns[0].ID != "workers" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestWorkday_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") == "1" {
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)

			return
		}
		off := r.URL.Query().Get("offset")
		switch off {
		case "0":
			_, _ = io.WriteString(w, `{"data":[{"id":"W1"}],"total":2}`)
		case "1":
			_, _ = io.WriteString(w, `{"data":[{"id":"W2"}],"total":2}`)
		default:
			_, _ = io.WriteString(w, `{"data":[],"total":2}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "workers"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "W1" || ids[1] != "W2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestWorkday_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/workers", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "workers"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestWorkday_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[],"total":0}`)
	})
	mux.HandleFunc("/workers/W1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"W1","fullName":"Alice","jobTitle":"Engineer","primaryEmail":"alice@x","hireDate":"2024-01-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "workers", ID: "W1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Alice" || doc.Author != "alice@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Engineer") {
		t.Fatalf("body=%q", body)
	}
}

func TestWorkday_DeltaSync_BootstrapReturnsLatestLastModified(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("order") == "-lastModifiedDateTime" {
			_, _ = io.WriteString(w, `{"data":[{"id":"W9","lastModifiedDateTime":"2024-06-30T12:00:00Z"}],"total":1}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[],"total":0}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "workers"}, "")
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

func TestWorkday_DeltaSync_TerminatedMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Updated_From") == "" {
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[
			{"id":"W1","active":true,"lastModifiedDateTime":"2024-06-01T00:00:00Z"},
			{"id":"W2","active":false,"lastModifiedDateTime":"2024-06-02T00:00:00Z","terminationDate":"2024-06-02T00:00:00Z"}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, newCur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "workers"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 || changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
	if newCur != "2024-06-02T00:00:00Z" {
		t.Fatalf("cursor=%q", newCur)
	}
}

func TestWorkday_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/workers", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "workers"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestWorkday_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := workday.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
