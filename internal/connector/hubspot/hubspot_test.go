package hubspot_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(hubspot.Credentials{AccessToken: "pat-test"})

	return b
}

func newHubServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func TestHubSpot_Validate(t *testing.T) {
	t.Parallel()
	h := hubspot.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := h.Validate(context.Background(), tc.cfg)
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

func TestHubSpot_Connect(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"portalId":42}`))
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, err := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestHubSpot_Connect_AuthFails(t *testing.T) {
	t.Parallel()
	srv := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	if _, err := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestHubSpot_ListNamespaces(t *testing.T) {
	t.Parallel()
	srv := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()), hubspot.WithObjects([]string{"contacts", "companies"}))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := h.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 2 || ns[0].Kind != "crm_object" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestHubSpot_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/crm/v3/objects/contacts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			_, _ = w.Write([]byte(`{"results":[{"id":"c1","updatedAt":"2024-01-01T00:00:00Z"}],"paging":{"next":{"after":"AFT"}}}`))

			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"c2","updatedAt":"2024-01-02T00:00:00Z"}]}`))
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := h.ListDocuments(context.Background(), conn, connector.Namespace{ID: "contacts"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %v", ids)
	}
}

func TestHubSpot_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/crm/v3/objects/contacts", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := h.ListDocuments(context.Background(), conn, connector.Namespace{ID: "contacts"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if it.Err() == nil || errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("expected non-EOP, got %v", it.Err())
	}
}

func TestHubSpot_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/crm/v3/objects/contacts/c1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"c1","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z","properties":{"name":"Alice"}}`))
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := h.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "contacts", ID: "c1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	b, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(b), "Alice") {
		t.Fatalf("body: %q", b)
	}
}

func TestHubSpot_DeltaSync(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/crm/v3/objects/contacts/search", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "filterGroups") {
			_, _ = w.Write([]byte(`{"results":[{"id":"c1","updatedAt":"2024-01-01T00:00:00Z","properties":{"hs_lastmodifieddate":"2024-01-01T00:00:00Z"}}]}`))

			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":"c2","updatedAt":"2024-01-02T00:00:00Z","properties":{"hs_lastmodifieddate":"2024-01-02T00:00:00Z"}}]}`))
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := h.DeltaSync(context.Background(), conn, connector.Namespace{ID: "contacts"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-01T00:00:00Z" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = h.DeltaSync(context.Background(), conn, connector.Namespace{ID: "contacts"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || cur != "2024-01-02T00:00:00Z" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

// TestHubSpot_DeltaSync_RateLimited locks in that a 429 during a
// delta sync surfaces as connector.ErrRateLimited so the adaptive
// rate limiter can react — matching the ListDocuments iterator.
func TestHubSpot_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/integrations/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/crm/v3/objects/contacts/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newHubServer(t, mux)
	h := hubspot.New(hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(srv.Client()))
	conn, _ := h.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := h.DeltaSync(context.Background(), conn, connector.Namespace{ID: "contacts"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestHubSpot_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()
	if _, err := hubspot.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestHubSpot_Registered(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(hubspot.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
