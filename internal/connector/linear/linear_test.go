package linear_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(linear.Credentials{APIKey: "lin_api_test"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

// gqlServer returns an httptest.Server that decodes the inbound
// GraphQL POST body, looks up the operation by a sub-string match
// on the query string, and returns the matching response.
func gqlServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		for needle, resp := range responses {
			if strings.Contains(req.Query, needle) {
				_, _ = w.Write([]byte(resp))

				return
			}
		}
		http.Error(w, "no match", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestLinear_Validate(t *testing.T) {
	t.Parallel()

	l := linear.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := l.Validate(context.Background(), tc.cfg)
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

func TestLinear_Connect(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer": `{"data":{"viewer":{"id":"U1"}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestLinear_Connect_AuthFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	if _, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestLinear_ListNamespaces(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer": `{"data":{"viewer":{"id":"U1"}}}`,
		"teams":  `{"data":{"teams":{"nodes":[{"id":"T1","key":"ENG","name":"Engineering"}]}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := l.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 1 || ns[0].Kind != "team" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestLinear_ListDocuments(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":       `{"data":{"viewer":{"id":"U1"}}}`,
		"issues(first": `{"data":{"team":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"I1","identifier":"ENG-1","updatedAt":"2024-01-01T00:00:00Z"},{"id":"I2","identifier":"ENG-2","updatedAt":"2024-01-02T00:00:00Z"}]}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := l.ListDocuments(context.Background(), conn, connector.Namespace{ID: "T1"}, connector.ListOpts{})
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

func TestLinear_FetchDocument(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":    `{"data":{"viewer":{"id":"U1"}}}`,
		"issue(id:": `{"data":{"issue":{"id":"I1","identifier":"ENG-1","title":"Test","description":"hello","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z","creator":{"name":"alice","email":"a@b.com"}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := l.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "T1", ID: "I1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello" {
		t.Fatalf("body: %q", body)
	}
	if doc.Metadata["identifier"] != "ENG-1" {
		t.Fatalf("meta: %+v", doc.Metadata)
	}
}

func TestLinear_DeltaSync(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":             `{"data":{"viewer":{"id":"U1"}}}`,
		"orderBy: updatedAt": `{"data":{"team":{"issues":{"nodes":[{"id":"I1","identifier":"ENG-1","updatedAt":"2024-01-02T00:00:00Z"},{"id":"I2","identifier":"ENG-2","updatedAt":"2024-01-03T00:00:00Z"}]}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-03T00:00:00Z" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 2 || cur != "2024-01-03T00:00:00Z" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

func TestLinear_HandleWebhook(t *testing.T) {
	t.Parallel()

	l := linear.New()
	for _, tc := range []struct {
		name    string
		payload string
		want    int
		kind    connector.ChangeKind
	}{
		{"create", `{"action":"create","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-01T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeUpserted},
		{"update", `{"action":"update","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-02T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeUpserted},
		{"remove", `{"action":"remove","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-03T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeDeleted},
		{"other-type", `{"action":"create","type":"Comment","data":{"id":"C1"}}`, 0, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changes, err := l.HandleWebhook(context.Background(), []byte(tc.payload))
			if err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if len(changes) != tc.want {
				t.Fatalf("count: got %d want %d", len(changes), tc.want)
			}
			if tc.want > 0 && changes[0].Kind != tc.kind {
				t.Fatalf("kind: got %d want %d", changes[0].Kind, tc.kind)
			}
		})
	}
}

func TestLinear_HandleWebhook_Empty(t *testing.T) {
	t.Parallel()

	l := linear.New()
	if _, err := l.HandleWebhook(context.Background(), nil); err == nil {
		t.Fatal("expected error on empty payload")
	}
}

func TestLinear_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	if _, err := linear.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestLinear_WebhookPath(t *testing.T) {
	t.Parallel()

	if got := linear.New().WebhookPath(); got != "/linear" {
		t.Fatalf("WebhookPath: %q", got)
	}
}

func TestLinear_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector(linear.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}

// rateLimitedGQLBody is Linear's documented 200-OK GraphQL
// rate-limit response shape (docs/runbooks/linear.md §3, §5):
// `{"errors":[{"message":"...","extensions":{"code":"RATELIMITED"}}]}`.
const rateLimitedGQLBody = `{"errors":[{"message":"per-key quota exhausted","extensions":{"code":"RATELIMITED"}}]}`

// TestLinear_Connect_RateLimitedOn200 verifies that a 200-OK
// response with `errors[0].extensions.code = "RATELIMITED"` on
// the `viewer { id }` connectivity check wraps as
// connector.ErrRateLimited. Before the fix the connector would
// return a generic GraphQL error and the adaptive rate limiter
// in internal/connector/adaptive_rate.go would be blind during
// Connect-time throttling.
func TestLinear_Connect_RateLimitedOn200(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer": rateLimitedGQLBody,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	_, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited from Connect, got %v", err)
	}
}

// TestLinear_ListDocuments_RateLimitedOn200 verifies the same
// behaviour on the document-iteration hot path. The audit gate
// in internal/connector/audit_test.go is textual; this test
// asserts the behavioural contract through errors.Is.
func TestLinear_ListDocuments_RateLimitedOn200(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":       `{"data":{"viewer":{"id":"U1"}}}`,
		"issues(first": rateLimitedGQLBody,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := l.ListDocuments(context.Background(), conn, connector.Namespace{ID: "T1"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	if it.Next(context.Background()) {
		t.Fatal("Next must not advance on 200-OK RATELIMITED")
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited from iterator, got %v", it.Err())
	}
}

// TestLinear_DeltaSync_RateLimitedOn200 verifies the same
// behaviour on DeltaSync. Before the fix DeltaSync's response
// struct silently dropped the `errors` array entirely (the
// field was absent), so a 200-OK RATELIMITED response returned
// zero changes plus an unchanged cursor and the adaptive rate
// limiter could not react during incremental sync.
func TestLinear_DeltaSync_RateLimitedOn200(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":             `{"data":{"viewer":{"id":"U1"}}}`,
		"orderBy: updatedAt": rateLimitedGQLBody,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited from DeltaSync, got %v", err)
	}
}

// TestLinear_DeltaSync_InitialCursorIsCurrent locks in that an
// initial DeltaSync call (cursor == "") bootstraps the cursor
// from the single most-recently-updated issue, not from the 50
// oldest. The previous query used `issues(first: 50, orderBy:
// updatedAt)` which defaults to ascending — newCursor became the
// 50th-oldest timestamp (potentially months stale) so the next
// call would backfill nearly the entire team's history as
// "changes", violating the DeltaSyncer contract.
//
// This test fails the regression by asserting:
//  1. The outgoing GraphQL query contains "Descending" (bootstrap
//     must sort newest-first to grab the latest record).
//  2. The outgoing query contains "first: 1" (bootstrap must not
//     fetch 50 records).
//  3. The outgoing query does NOT contain an "updatedAt" filter
//     (bootstrap returns the current cursor, not pre-cursor history).
//  4. newCursor equals the most-recently-updated issue's
//     updatedAt — not an old timestamp from the back of the queue.
func TestLinear_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()

	var seenQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		seenQueries = append(seenQueries, req.Query)
		switch {
		case strings.Contains(req.Query, "viewer"):
			_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"U1"}}}`))
		case strings.Contains(req.Query, "issues"):
			// Serve only the most-recently-updated issue — the cursor
			// must equal its updatedAt, NOT a stale 50th-oldest value.
			_, _ = w.Write([]byte(`{"data":{"team":{"issues":{"nodes":[{"id":"latest","identifier":"ENG-LATEST","updatedAt":"2024-12-31T23:59:59Z"}]}}}}`))
		default:
			http.Error(w, "no match", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	changes, cur, err := l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial DeltaSync must not emit changes (empty cursor = bootstrap only), got %d", len(changes))
	}
	if cur != "2024-12-31T23:59:59Z" {
		t.Fatalf("initial cursor must equal the most-recently-updated issue's updatedAt, got %q", cur)
	}

	// Find the issues query (skip the viewer connectivity check).
	var issuesQuery string
	for _, q := range seenQueries {
		if strings.Contains(q, "issues") {
			issuesQuery = q

			break
		}
	}
	if issuesQuery == "" {
		t.Fatal("expected an issues GraphQL query to be issued during bootstrap")
	}
	if !strings.Contains(issuesQuery, "Descending") {
		t.Errorf("initial DeltaSync must sort Descending to bootstrap from the most-recently-updated issue, got query=%s", issuesQuery)
	}
	if !strings.Contains(issuesQuery, "first: 1") {
		t.Errorf("initial DeltaSync must use first: 1 (not 50) so cursor reflects 'now', got query=%s", issuesQuery)
	}
	if strings.Contains(issuesQuery, "updatedAt") && strings.Contains(issuesQuery, "$filter") {
		t.Errorf("initial DeltaSync must not send an updatedAt filter (bootstrap = no pre-cursor backfill), got query=%s", issuesQuery)
	}
}
