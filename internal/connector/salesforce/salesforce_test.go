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

// TestSalesforce_DeltaSync_RejectsTamperedCursor locks in that the
// cursor is parsed and re-formatted as time.RFC3339 before being
// concatenated into the SOQL WHERE clause. A tampered cursor row
// (e.g. an injected SOQL clause stored in place of the timestamp)
// must surface as connector.ErrInvalidConfig and never reach the
// /query endpoint, defending against SOQL injection via the
// platform-managed cursor.
func TestSalesforce_DeltaSync_RejectsTamperedCursor(t *testing.T) {
	t.Parallel()
	bad := []string{
		"' OR Id != null --",
		"2024-01-01T00:00:00Z; DROP TABLE Account",
		"2024-01-01",       // date-only, not full RFC3339
		"2024-01-01 00:00", // space instead of T
		"not-a-timestamp",
		"", // empty is fine — caller short-circuits, handled below
	}
	for _, cur := range bad {
		cur := cur
		t.Run(cur, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{}`))
			})
			mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, r *http.Request) {
				// Empty cursor is the only valid case in `bad` — it
				// goes through and returns a benign first-call payload.
				if cur == "" {
					_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"a","SystemModstamp":"2024-01-01T00:00:00Z"}]}`))

					return
				}
				t.Errorf("unsanitised cursor %q reached /query as %q", cur, r.URL.Query().Get("q"))
				http.Error(w, "should not be called", http.StatusInternalServerError)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
			conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
			_, _, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, cur)
			if cur == "" {
				if err != nil {
					t.Fatalf("empty cursor must be accepted, got %v", err)
				}

				return
			}
			if !errors.Is(err, connector.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig for cursor %q, got %v", cur, err)
			}
		})
	}
}

// TestSalesforce_DeltaSync_InitialCursorIsCurrent locks in that an
// initial DeltaSync call (cursor == "") bootstraps the cursor from
// the single most-recently-modified record, not the page of
// oldest records. Salesforce's /query endpoint caps responses at
// 2000 records per page; the previous SOQL was
//
//	SELECT Id, SystemModstamp FROM <SObject> ORDER BY SystemModstamp ASC
//
// which made newCursor the 2000th-oldest timestamp — years stale
// for any org of meaningful size, causing the next call to backfill
// nearly the entire dataset as "changes" via `WHERE SystemModstamp
// > <stale>`. The DeltaSyncer contract says an empty cursor returns
// the current cursor without backfilling.
//
// This test fails the regression by asserting:
//  1. The outgoing SOQL contains `ORDER BY SystemModstamp DESC`.
//  2. The outgoing SOQL contains `LIMIT 1`.
//  3. The outgoing SOQL has NO `WHERE` clause on the bootstrap call.
//  4. newCursor equals the most-recent record's SystemModstamp,
//     even when only that single record is served by the mock.
func TestSalesforce_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var seenSOQL []string
	mux := http.NewServeMux()
	mux.HandleFunc("/services/data/v59.0/limits", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/services/data/v59.0/query", func(w http.ResponseWriter, r *http.Request) {
		soql := r.URL.Query().Get("q")
		seenSOQL = append(seenSOQL, soql)
		// Serve only the most-recent record — newCursor must equal
		// its SystemModstamp, not a stale 2000th-oldest timestamp.
		_, _ = w.Write([]byte(`{"done":true,"records":[{"Id":"latest","SystemModstamp":"2024-12-31T23:59:59Z"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := salesforce.New(salesforce.WithHTTPClient(srv.Client()), salesforce.WithBaseURL(srv.URL))
	conn, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	changes, cur, err := s.DeltaSync(context.Background(), conn, connector.Namespace{ID: "Account"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial DeltaSync must not emit changes (empty cursor = bootstrap only), got %d", len(changes))
	}
	if cur != "2024-12-31T23:59:59Z" {
		t.Fatalf("initial cursor must equal the most-recent record's SystemModstamp, got %q", cur)
	}
	if len(seenSOQL) == 0 {
		t.Fatal("expected a SOQL query to be issued during bootstrap")
	}
	soql := seenSOQL[0]
	if !strings.Contains(soql, "ORDER BY SystemModstamp DESC") {
		t.Errorf("initial DeltaSync must ORDER BY SystemModstamp DESC to bootstrap from the most-recent record, got soql=%q", soql)
	}
	if !strings.Contains(soql, "LIMIT 1") {
		t.Errorf("initial DeltaSync must LIMIT 1 so cursor reflects 'now', got soql=%q", soql)
	}
	if strings.Contains(soql, "WHERE") {
		t.Errorf("initial DeltaSync must not send a WHERE clause (bootstrap = no pre-cursor backfill), got soql=%q", soql)
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
