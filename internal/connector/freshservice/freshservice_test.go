package freshservice_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/freshservice"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(freshservice.Credentials{Domain: "acme", APIKey: "k"})

	return b
}

func TestFreshservice_Validate(t *testing.T) {
	t.Parallel()
	c := freshservice.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"domain":"d"}`)}, false},
		{"no domain", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_key":"k"}`)}, false},
		{"no tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"no source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds(t)}, false},
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

func TestFreshservice_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestFreshservice_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tickets":[{"id":11,"subject":"Outage","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/api/v2/tickets/11", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ticket":{"id":11,"subject":"Outage","description_text":"down","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
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
	if len(ids) != 1 || ids[0] != "11" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "tickets", ID: "11"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "down") {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
	if err := c.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestFreshservice_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestFreshservice_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updated_since") == "" {
			http.Error(w, "missing updated_since", http.StatusBadRequest)

			return
		}
		_, _ = io.WriteString(w, `{"tickets":[{"id":12,"updated_at":"2024-06-02T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "2024-05-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "12" {
		t.Fatalf("changes=%v", changes)
	}
	if cur == "" {
		t.Fatalf("cur empty")
	}
}

// TestFreshservice_ListDocuments_LastPageYieldsAllItems regresses a
// Round-24 bug in docIterator.Next where the `done` flag was checked
// at the top of the method. Because fetch() sets `done=true` on the
// last page (Link header missing OR len(page) < perPage), the first
// call after the final fetch returned item 0 but the next call hit
// `done==true` and bailed without serving items 1..N-1. The fix moves
// the `done` check inside the `idx >= len(page)` branch.
func TestFreshservice_ListDocuments_LastPageYieldsAllItems(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	// Three tickets returned in one shot. No Link header → the
	// iterator treats this as the last page and sets done=true on
	// the same fetch that loaded the items.
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tickets":[`+
			`{"id":1,"updated_at":"2024-06-01T00:00:00Z"},`+
			`{"id":2,"updated_at":"2024-06-02T00:00:00Z"},`+
			`{"id":3,"updated_at":"2024-06-03T00:00:00Z"}`+
			`]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "tickets"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(ids) != 3 || ids[0] != "1" || ids[1] != "2" || ids[2] != "3" {
		t.Fatalf("last-page truncation: ids=%v, want [1 2 3]", ids)
	}
}

func TestFreshservice_RateLimited_OnList(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshservice.New(freshservice.WithBaseURL(srv.URL), freshservice.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "tickets"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}
