package servicenow_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/servicenow"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(servicenow.Credentials{Instance: "acme", Username: "u", Password: "p"})

	return b
}

func TestServiceNow_Validate(t *testing.T) {
	t.Parallel()
	c := servicenow.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no instance", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"username":"u","password":"p"}`)}, false},
		{"no auth", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"instance":"acme"}`)}, false},
		{"oauth ok", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"instance":"acme","access_token":"oat"}`)}, true},
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

func TestServiceNow_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestServiceNow_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sysparm_limit") == "1" {
			_, _ = io.WriteString(w, `{"result":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"result":[{"sys_id":"abc","number":"INC0001","short_description":"oops","sys_updated_on":"2024-06-01 00:00:00","sys_created_on":"2024-05-01 00:00:00"}]}`)
	})
	mux.HandleFunc("/api/now/table/incident/abc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"result":{"sys_id":"abc","number":"INC0001","short_description":"oops","description":"body","sys_updated_on":"2024-06-01 00:00:00","sys_created_on":"2024-05-01 00:00:00"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 1 || ns[0].ID != "incident" {
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
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "incident", ID: "abc"})
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
	if err := c.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestServiceNow_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"result":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incident"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestServiceNow_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("sysparm_query")
		if !strings.HasPrefix(q, "sys_updated_on>") {
			_, _ = io.WriteString(w, `{"result":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"result":[{"sys_id":"abc","sys_updated_on":"2024-06-02 00:00:00"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incident"}, "2024-06-01 00:00:00")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || cur != "2024-06-02 00:00:00" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestServiceNow_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sysparm_limit") == "1" {
			_, _ = io.WriteString(w, `{"result":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incident"}, "2024-06-01 00:00:00")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestServiceNow_ListDocuments_PaginatesAllPages verifies the
// Round-22 pagination fix: ListDocuments must advance the
// `sysparm_offset` cursor until the API returns fewer than
// `sysparm_limit` records.
func TestServiceNow_ListDocuments_PaginatesAllPages(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("sysparm_offset")
		switch offset {
		case "", "0":
			// First page: fabricate 100 result rows so the
			// connector advances the offset to the next page.
			var b strings.Builder
			b.WriteString(`{"result":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"sys_id":"p1-`)
				b.WriteString(strconv.Itoa(i))
				b.WriteString(`","sys_updated_on":"2024-06-01 00:00:00"}`)
			}
			b.WriteString(`]}`)
			_, _ = io.WriteString(w, b.String())
		case "100":
			_, _ = io.WriteString(w, `{"result":[{"sys_id":"p2-0","sys_updated_on":"2024-06-02 00:00:00"}]}`)
		default:
			http.Error(w, "unexpected offset "+offset, http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "incident"}, connector.ListOpts{})
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

func TestServiceNow_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(servicenow.Name); err != nil {
		t.Fatalf("registry missing servicenow: %v", err)
	}
}
