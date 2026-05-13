package freshdesk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/freshdesk"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(freshdesk.Credentials{Domain: "acme", APIKey: "k"})

	return b
}

func TestFreshdesk_Validate(t *testing.T) {
	t.Parallel()
	c := freshdesk.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"domain":"d"}`)}, false},
		{"no domain", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_key":"k"}`)}, false},
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

func TestFreshdesk_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestFreshdesk_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":7,"subject":"Bug","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}]`)
	})
	mux.HandleFunc("/api/v2/tickets/7", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":7,"subject":"Bug","description_text":"body","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
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
	if len(ids) != 1 || ids[0] != "7" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "tickets", ID: "7"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "body") {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
}

func TestFreshdesk_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestFreshdesk_DeltaSync_Incremental(t *testing.T) {
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
		_, _ = io.WriteString(w, `[{"id":8,"updated_at":"2024-06-02T00:00:00Z"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || cur != "2024-06-02T00:00:00Z" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestFreshdesk_DeltaSync_RateLimited(t *testing.T) {
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
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "2024-06-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestFreshdesk_ListDocuments_PaginatesAllPages verifies the
// Round-22 pagination fix: ListDocuments must walk the 1-indexed
// `?page=N` parameter until Freshdesk returns fewer than
// `per_page` records.
func TestFreshdesk_ListDocuments_PaginatesAllPages(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			var b strings.Builder
			b.WriteString(`[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"id":`)
				b.WriteString(strconv.Itoa(i))
				b.WriteString(`,"updated_at":"2024-06-01T00:00:00Z"}`)
			}
			b.WriteString(`]`)
			_, _ = io.WriteString(w, b.String())
		case "2":
			_, _ = io.WriteString(w, `[{"id":1000,"updated_at":"2024-06-02T00:00:00Z"}]`)
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "tickets"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	n := 0
	for it.Next(context.Background()) {
		n++
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if n != 101 {
		t.Fatalf("expected 101 records across 2 pages, got %d", n)
	}
}

// TestFreshdesk_DeltaSync_PaginatesAllPages exercises the Round-23
// Devin Review fix: DeltaSync must follow the 1-indexed `page=N`
// parameter beyond the first 100 tickets. The mock sends a Link
// header on page 1 only so EOF is also detected without firing a
// trailing empty request.
func TestFreshdesk_DeltaSync_PaginatesAllPages(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	calls := 0
	mux.HandleFunc("/api/v2/tickets", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updated_since") == "" {
			http.Error(w, "missing updated_since", http.StatusBadRequest)

			return
		}
		calls++
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("Link", `<https://api.example.com/api/v2/tickets?page=2>; rel="next"`)
			var b strings.Builder
			b.WriteString(`[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"id":`)
				b.WriteString(strconv.Itoa(i))
				b.WriteString(`,"updated_at":"2024-06-02T00:00:00Z"}`)
			}
			b.WriteString(`]`)
			_, _ = io.WriteString(w, b.String())
		case "2":
			_, _ = io.WriteString(w, `[{"id":1000,"updated_at":"2024-06-03T00:00:00Z"}]`)
		case "3":
			t.Fatalf("page 3 should not be requested — short page on page 2 is EOF")
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := freshdesk.New(freshdesk.WithBaseURL(srv.URL), freshdesk.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 page requests, got %d", calls)
	}
	if len(changes) != 101 {
		t.Fatalf("expected 101 changes across 2 pages, got %d", len(changes))
	}
	if cur != "2024-06-03T00:00:00Z" {
		t.Fatalf("cur=%q want 2024-06-03T00:00:00Z (newest across pages)", cur)
	}
}

func TestFreshdesk_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(freshdesk.Name); err != nil {
		t.Fatalf("registry missing freshdesk: %v", err)
	}
}
