package salesforce_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
)

func validCreds(t *testing.T, instance string) []byte {
	t.Helper()
	b, _ := json.Marshal(salesforce.Credentials{InstanceURL: instance, AccessToken: "00D"})

	return b
}

func TestSalesforce_Validate(t *testing.T) {
	t.Parallel()
	s := salesforce.New()
	good, _ := json.Marshal(salesforce.Credentials{InstanceURL: "https://x", AccessToken: "t"})
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: good}, true},
		{"empty", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing instance", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"t"}`)}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"instance_url":"x"}`)}, false},
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

func TestSalesforce_Connect(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestSalesforce_Connect_AuthFails(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	if _, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestSalesforce_ListNamespaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL), salesforce.WithObjects([]string{"Account", "Contact"}))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	ns, err := s.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 2 || ns[0].Kind != "sobject" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestSalesforce_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	calls := 0
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"done":false,"nextRecordsUrl":"/services/data/v59.0/query/01g000000000001-2000","records":[{"Id":"a","SystemModstamp":"2024-01-01T00:00:00Z"}]}`))

			return
		}
		_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"b","SystemModstamp":"2024-01-02T00:00:00Z"}]}`))
	})
	mux.HandleFunc("/services/data/v59.0/query/01g000000000001-2000", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"b","SystemModstamp":"2024-01-02T00:00:00Z"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "Account"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %v calls: %d", ids, calls)
	}
}

func TestSalesforce_ListDocuments_Empty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"done":true,"records":[]}`))
	}))
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "Account"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
		t.Fatal("expected no docs")
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
}

func TestSalesforce_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "Account"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if it.Err() == nil || errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("expected non-EOP, got %v", it.Err())
	}
}

func TestSalesforce_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/services/data/v59.0/sobjects/Account/a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Id":"a","Name":"Acme","SystemModstamp":"2024-01-02T00:00:00Z","CreatedDate":"2024-01-01T00:00:00Z"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := s.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "Account", ID: "a"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Acme") {
		t.Fatalf("body: %q", body)
	}
	if doc.Metadata["sobject"] != "Account" {
		t.Fatalf("meta: %+v", doc.Metadata)
	}
}

func TestSalesforce_DeltaSync(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, r *http.Request) {
		// First call: no WHERE; second call: WHERE present.
		if !strings.Contains(r.URL.Query().Get("q"), "WHERE") {
			_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"a","SystemModstamp":"2024-01-01T00:00:00Z"}]}`))

			return
		}
		_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"b","SystemModstamp":"2024-01-02T00:00:00Z"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-01T00:00:00Z" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || cur != "2024-01-02T00:00:00Z" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

// TestSalesforce_DeltaSync_RateLimited locks in that a 429 during a
// delta sync surfaces as connector.ErrRateLimited so the adaptive
// rate limiter in internal/connector/adaptive_rate.go can react —
// matching the behaviour of the ListDocuments iterator.
func TestSalesforce_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, _, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestSalesforce_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()
	if _, err := salesforce.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestSalesforce_Registered(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(salesforce.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
