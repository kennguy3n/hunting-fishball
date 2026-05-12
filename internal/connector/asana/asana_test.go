package asana_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/asana"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(asana.Credentials{AccessToken: "as-test"})

	return b
}

func newAsanaServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func TestAsana_Validate(t *testing.T) {
	t.Parallel()
	a := asana.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := a.Validate(context.Background(), tc.cfg)
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

func TestAsana_Connect(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no token", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, err := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestAsana_Connect_AuthFails(t *testing.T) {
	t.Parallel()
	srv := newAsanaServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	if _, err := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected error")
	}
}

func TestAsana_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"gid":"W1","name":"Acme"}]}`))
	})
	mux.HandleFunc("/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "" {
			_, _ = w.Write([]byte(`{"data":[{"gid":"P1","name":"Backend"}],"next_page":{"offset":"NEXT"}}`))

			return
		}
		_, _ = w.Write([]byte(`{"data":[{"gid":"P2","name":"Frontend"}]}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := a.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 2 || ns[0].Kind != "project" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestAsana_ListDocuments(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"gid":"T1","name":"a","modified_at":"2024-01-01T00:00:00Z"},{"gid":"T2","name":"b","modified_at":"2024-01-02T00:00:00Z"}]}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := a.ListDocuments(context.Background(), conn, connector.Namespace{ID: "P1"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
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

func TestAsana_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := a.ListDocuments(context.Background(), conn, connector.Namespace{ID: "P1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if it.Err() == nil || errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("expected non-EOP, got %v", it.Err())
	}
}

func TestAsana_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/tasks/T1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"T1","name":"task","notes":"detail","created_at":"2024-01-01T00:00:00Z","modified_at":"2024-01-02T00:00:00Z"}}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := a.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "P1", ID: "T1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	b, _ := io.ReadAll(doc.Content)
	if string(b) != "detail" {
		t.Fatalf("body: %q", b)
	}
}

func TestAsana_DeltaSync(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	// Bootstrap path: cursor=="" → workspace search endpoint with
	// sort_by=modified_at desc + limit=1 returns the single
	// most-recently-modified task across the project. newCursor
	// MUST equal that task's modified_at — "now" — not the
	// 100th-oldest-by-creation timestamp.
	mux.HandleFunc("/workspaces/W1/tasks/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"gid":"latest","modified_at":"2024-12-31T23:59:59Z"}]}`))
	})
	// Steady-state path: cursor!="" → project tasks endpoint with
	// modified_since.
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"gid":"T3","modified_at":"2024-01-04T00:00:00Z"}]}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns := connector.Namespace{ID: "P1", Metadata: map[string]string{"workspace_gid": "W1", "project_gid": "P1"}}
	changes, cur, err := a.DeltaSync(context.Background(), conn, ns, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-12-31T23:59:59Z" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = a.DeltaSync(context.Background(), conn, ns, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || cur != "2024-01-04T00:00:00Z" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

// TestAsana_DeltaSync_InitialCursorIsCurrent locks in that an
// initial DeltaSync call (cursor == "") bootstraps the cursor from
// the single most-recently-modified task via the workspace search
// endpoint, not from the first 100 creation-ordered tasks on
// /projects/{gid}/tasks. The previous query sent
// `/projects/{gid}/tasks?limit=100` with no sort — Asana's
// /projects/{gid}/tasks endpoint orders by creation / task-list
// rank (NOT modified_at), so newCursor became the
// max(modified_at) over the first 100 creation-ordered tasks,
// which for any active project where the recently-modified task
// isn't in the first creation-ordered page is stale. The next
// call's modified_since filter would then backfill almost the
// whole project as "changes", violating the DeltaSyncer contract.
//
// This test fails the regression by asserting:
//  1. The outgoing request path is /workspaces/<wgid>/tasks/search
//     (NOT /projects/<gid>/tasks) — only the workspace search
//     endpoint supports server-side sort_by=modified_at.
//  2. The outgoing query carries projects.any=<project_gid>.
//  3. The outgoing query carries sort_by=modified_at and
//     sort_ascending=false (bootstrap must sort newest-first).
//  4. The outgoing query carries limit=1 (bootstrap must not
//     fetch 100 records).
//  5. The outgoing query does NOT carry modified_since (bootstrap
//     returns the current cursor, not pre-cursor history).
//  6. newCursor equals the most-recently-modified task's
//     modified_at — not an old timestamp from the back of the
//     creation-ordered queue.
func TestAsana_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var seenPaths []string
	var seenQueries []string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/workspaces/W1/tasks/search", func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		seenQueries = append(seenQueries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"data":[{"gid":"latest","modified_at":"2024-12-31T23:59:59Z"}]}`))
	})
	// If the regressed code calls /projects/P1/tasks during the
	// bootstrap path, record that too so the failure is loud.
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		seenQueries = append(seenQueries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"data":[{"gid":"oldest","modified_at":"2020-01-01T00:00:00Z"}]}`))
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, err := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns := connector.Namespace{ID: "P1", Metadata: map[string]string{"workspace_gid": "W1", "project_gid": "P1"}}
	changes, cur, err := a.DeltaSync(context.Background(), conn, ns, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial DeltaSync must not emit changes (empty cursor = bootstrap only), got %d", len(changes))
	}
	if cur != "2024-12-31T23:59:59Z" {
		t.Fatalf("initial cursor must equal the most-recently-modified task's modified_at, got %q", cur)
	}
	if len(seenPaths) == 0 {
		t.Fatal("expected an HTTP request to be issued during bootstrap")
	}
	path := seenPaths[0]
	query := seenQueries[0]
	if path != "/workspaces/W1/tasks/search" {
		t.Errorf("initial DeltaSync must hit /workspaces/<wgid>/tasks/search to bootstrap from the most-recent record, got path=%q", path)
	}
	if !strings.Contains(query, "projects.any=P1") {
		t.Errorf("initial DeltaSync must filter projects.any=<project_gid>, got query=%q", query)
	}
	if !strings.Contains(query, "sort_by=modified_at") {
		t.Errorf("initial DeltaSync must sort_by=modified_at to bootstrap from the most-recent record, got query=%q", query)
	}
	if !strings.Contains(query, "sort_ascending=false") {
		t.Errorf("initial DeltaSync must sort_ascending=false to bootstrap from the most-recent record, got query=%q", query)
	}
	if !strings.Contains(query, "limit=1") {
		t.Errorf("initial DeltaSync must limit=1 so cursor reflects 'now', got query=%q", query)
	}
	if strings.Contains(query, "modified_since") {
		t.Errorf("initial DeltaSync must not send a modified_since filter (bootstrap = no pre-cursor backfill), got query=%q", query)
	}
}

// TestAsana_DeltaSync_RateLimited locks in that a 429 during a delta
// sync surfaces as connector.ErrRateLimited on BOTH the bootstrap
// path (workspace search) and the steady-state path (project
// tasks), so the adaptive rate limiter reacts regardless of which
// code path Asana throttled — matching the ListDocuments iterator.
func TestAsana_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gid":"U1"}}`))
	})
	mux.HandleFunc("/workspaces/W1/tasks/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	mux.HandleFunc("/projects/P1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newAsanaServer(t, mux)
	a := asana.New(asana.WithBaseURL(srv.URL), asana.WithHTTPClient(srv.Client()))
	conn, _ := a.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns := connector.Namespace{ID: "P1", Metadata: map[string]string{"workspace_gid": "W1", "project_gid": "P1"}}
	_, _, err := a.DeltaSync(context.Background(), conn, ns, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited on bootstrap, got %v", err)
	}
	_, _, err = a.DeltaSync(context.Background(), conn, ns, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited on steady-state, got %v", err)
	}
}

func TestAsana_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()
	if _, err := asana.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestAsana_Registered(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(asana.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
