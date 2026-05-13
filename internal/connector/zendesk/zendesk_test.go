package zendesk_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/zendesk"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(zendesk.Credentials{Subdomain: "acme", Email: "agent@acme.com", APIToken: "tok"})

	return b
}

func TestZendesk_Validate(t *testing.T) {
	t.Parallel()
	c := zendesk.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"subdomain":"acme","email":"a@b"}`)}, false},
		{"token without email", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"subdomain":"acme","api_token":"tok"}`)}, false},
		{"oauth ok", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"subdomain":"acme","access_token":"oat"}`)}, true},
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

func TestZendesk_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestZendesk_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{"id":1}}`)
	})
	mux.HandleFunc("/api/v2/tickets.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tickets":[{"id":42,"subject":"Spec","updated_at":"2024-06-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/api/v2/tickets/42.json", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			http.Error(w, "no auth", http.StatusUnauthorized)

			return
		}
		_, _ = io.WriteString(w, `{"ticket":{"id":42,"subject":"Spec","description":"body","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 1 || ns[0].ID != "tickets" {
		t.Fatalf("ns=%+v", ns)
	}
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(ids) != 1 || ids[0] != "42" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "tickets", ID: "42"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "body" {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
	if err := c.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestZendesk_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestZendesk_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v2/incremental/tickets.json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start_time") == "" {
			http.Error(w, "missing start_time", http.StatusBadRequest)

			return
		}
		_, _ = io.WriteString(w, `{"tickets":[{"id":7,"updated_at":"2024-06-02T00:00:00Z"}],"end_time":1717286400}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "1717200000")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || cur != "1717286400" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestZendesk_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v2/incremental/tickets.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "1717200000")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestZendesk_ListDocuments_FollowsNextPage verifies the Round-22
// pagination fix: ListDocuments must follow the `next_page` URL
// returned by Zendesk until the API stops emitting one, instead
// of stopping after the first page.
func TestZendesk_ListDocuments_FollowsNextPage(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{"id":1}}`)
	})
	var srvURL string
	mux.HandleFunc("/api/v2/tickets.json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = io.WriteString(w, `{"tickets":[{"id":2,"updated_at":"2024-06-02T00:00:00Z"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"tickets":[{"id":1,"updated_at":"2024-06-01T00:00:00Z"}],"next_page":"`+srvURL+`/api/v2/tickets.json?page=2"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
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
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("expected 2 IDs across 2 pages, got %v", ids)
	}
}

func TestZendesk_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(zendesk.Name); err != nil {
		t.Fatalf("registry missing zendesk: %v", err)
	}
}
