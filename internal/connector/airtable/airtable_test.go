package airtable_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/airtable"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(airtable.Credentials{AccessToken: "pat", BaseID: "appXYZ", Table: "Tasks"})

	return b
}

func TestAirtable_Validate(t *testing.T) {
	t.Parallel()
	c := airtable.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_id":"appX","table":"t"}`)}, false},
		{"no base", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"pat","table":"t"}`)}, false},
		{"no table", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"pat","base_id":"appX"}`)}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && !errors.Is(err, connector.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestAirtable_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestAirtable_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/appXYZ/Tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"records":[{"id":"rec1","createdTime":"2024-06-01T00:00:00Z","fields":{"Name":"Spec"}}]}`)
	})
	mux.HandleFunc("/v0/appXYZ/Tasks/rec1", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no auth", http.StatusUnauthorized)

			return
		}
		_, _ = io.WriteString(w, `{"id":"rec1","createdTime":"2024-06-01T00:00:00Z","fields":{"Name":"Spec"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(ids) != 1 || ids[0] != "rec1" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: ns[0].ID, ID: "rec1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Spec") {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
}

func TestAirtable_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/appXYZ/Tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"records":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "appXYZ/Tasks"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestAirtable_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/appXYZ/Tasks", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "filterByFormula") {
			_, _ = io.WriteString(w, `{"records":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"records":[{"id":"rec9","createdTime":"2024-06-02T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	before := time.Now().UTC()
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "appXYZ/Tasks"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes=%v", changes)
	}
	// Cursor must advance to a wall-clock timestamp captured at the
	// start of the call (the record payload only echoes createdTime;
	// LAST_MODIFIED_TIME() is filter-only, not returned per record).
	got, err := time.Parse(time.RFC3339, cur)
	if err != nil {
		t.Fatalf("cursor parse: %v (cur=%q)", err, cur)
	}
	if got.Before(before.Truncate(time.Second)) || got.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("cursor %q not within [%s, now]", cur, before.Format(time.RFC3339))
	}
}

func TestAirtable_DeltaSync_PaginatesAllPages(t *testing.T) {
	t.Parallel()
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/appXYZ/Tasks", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		switch r.URL.Query().Get("offset") {
		case "":
			_, _ = io.WriteString(w, `{"records":[{"id":"rec1","createdTime":"2024-06-02T00:00:00Z"}],"offset":"off-2"}`)
		case "off-2":
			_, _ = io.WriteString(w, `{"records":[{"id":"rec2","createdTime":"2024-06-03T00:00:00Z"}]}`)
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	atomic.StoreInt32(&calls, 0) // ignore the Connect() ping
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "appXYZ/Tasks"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes across 2 pages, got %d", len(changes))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 HTTP calls (one per page), got %d", got)
	}
}

func TestAirtable_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/appXYZ/Tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("maxRecords") == "1" {
			_, _ = io.WriteString(w, `{"records":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := airtable.New(airtable.WithBaseURL(srv.URL), airtable.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "appXYZ/Tasks"}, "2024-06-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestAirtable_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(airtable.Name); err != nil {
		t.Fatalf("registry missing airtable: %v", err)
	}
}
