package workday_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// TestWorkday_DeltaSync_PaginationAggregates pins the
// offset-based pagination loop in DeltaSync. Workday's
// /workers endpoint returns at most 200 rows per page and a
// `total` row count; the connector must walk every page in
// the Updated_From window before advancing the cursor, or
// else any change past row 200 with a lastModifiedDateTime
// <= the truncated page's newest entry is silently skipped on
// the next sync. This regresses Devin Review's Round-17
// finding on workday.go:360-401.
func TestWorkday_DeltaSync_PaginationAggregates(t *testing.T) {
	t.Parallel()
	const total = 203
	var (
		page1Hits atomic.Int32
		page2Hits atomic.Int32
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/workers", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("Updated_From") == "" {
			// Bootstrap probe — keep out of pagination counters.
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)

			return
		}
		off := q.Get("offset")
		switch off {
		case "", "0":
			page1Hits.Add(1)
			// Page 1: exactly 200 rows so the loop must hit the
			// server again. Every 10th row is terminated to also
			// verify ChangeDeleted aggregation across pages.
			var b strings.Builder
			b.WriteString(`{"data":[`)
			for i := 1; i <= 200; i++ {
				if i > 1 {
					b.WriteString(",")
				}
				active := "true"
				term := ""
				if i%10 == 0 {
					active = "false"
					term = `,"terminationDate":"2024-06-15T00:00:00Z"`
				}
				b.WriteString(fmt.Sprintf(
					`{"id":"W%d","active":%s,"lastModifiedDateTime":"2024-06-%02dT00:00:00Z"%s}`,
					i, active, (i%28)+1, term,
				))
			}
			b.WriteString(fmt.Sprintf(`],"total":%d}`, total))
			_, _ = io.WriteString(w, b.String())
		case "200":
			page2Hits.Add(1)
			// Page 2: 3 short rows. Includes a newer
			// lastModifiedDateTime than anything on page 1 to
			// pin that newestSeen is tracked across pages.
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"data":[
					{"id":"W201","active":true,"lastModifiedDateTime":"2024-06-29T00:00:00Z"},
					{"id":"W202","active":false,"lastModifiedDateTime":"2024-06-30T00:00:00Z","terminationDate":"2024-06-30T00:00:00Z"},
					{"id":"W203","active":true,"lastModifiedDateTime":"2024-07-01T00:00:00Z"}
				],"total":%d}`, total))
		default:
			t.Errorf("unexpected pagination offset=%q", off)
			_, _ = io.WriteString(w, `{"data":[],"total":0}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := workday.New(workday.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	changes, newCur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "workers"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if got := page1Hits.Load(); got != 1 {
		t.Fatalf("page1 hit count: got %d want 1", got)
	}
	if got := page2Hits.Load(); got != 1 {
		t.Fatalf("page2 hit count: got %d want 1", got)
	}
	if got := len(changes); got != total {
		t.Fatalf("aggregated change count: got %d want %d", got, total)
	}
	// newestSeen must reflect the page-2 newest row, not page 1.
	if newCur != "2024-07-01T00:00:00Z" {
		t.Fatalf("cursor: got %q want 2024-07-01T00:00:00Z", newCur)
	}
	last := changes[len(changes)-1]
	if last.Ref.ID != "W203" {
		t.Fatalf("tail change: got %+v want id=W203", last)
	}
	// Verify a deleted row from page 2 made it into the slice.
	var sawW202Deleted bool
	for _, ch := range changes {
		if ch.Ref.ID == "W202" && ch.Kind == connector.ChangeDeleted {
			sawW202Deleted = true

			break
		}
	}
	if !sawW202Deleted {
		t.Fatal("expected ChangeDeleted for terminated worker W202 (page 2)")
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
