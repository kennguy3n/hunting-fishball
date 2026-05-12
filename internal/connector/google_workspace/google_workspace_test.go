package googleworkspace_test

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
	googleworkspace "github.com/kennguy3n/hunting-fishball/internal/connector/google_workspace"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(googleworkspace.Credentials{AccessToken: "ya29.test"})

	return b
}

func TestGoogleWorkspace_Validate(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
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

func TestGoogleWorkspace_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGoogleWorkspace_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 2 {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestGoogleWorkspace_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("maxResults") == "1" {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		if r.URL.Query().Get("pageToken") == "n1" {
			_, _ = io.WriteString(w, `{"users":[{"id":"U2","primaryEmail":"bob@x"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[{"id":"U1","primaryEmail":"alice@x"}],"nextPageToken":"n1"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "U1" || ids[1] != "U2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestGoogleWorkspace_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestGoogleWorkspace_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"users":[]}`)
	})
	mux.HandleFunc("/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","primaryEmail":"alice@acme.com","creationTime":"2024-01-01T00:00:00Z","name":{"fullName":"Alice"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "users", ID: "U1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "alice@acme.com" {
		t.Fatalf("doc.Title=%q", doc.Title)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Alice") {
		t.Fatalf("body=%q", body)
	}
}

func TestGoogleWorkspace_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sortOrder") == "DESCENDING" {
			_, _ = io.WriteString(w, `{"users":[{"id":"U1","primaryEmail":"alice@x"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur == "" {
		t.Fatalf("expected non-empty bootstrap cursor")
	}
}

func TestGoogleWorkspace_DeltaSync_SuspendedMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updatedMin") == "" {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"users":[
			{"id":"U1","primaryEmail":"alice@x","suspended":false},
			{"id":"U2","primaryEmail":"bob@x","suspended":true}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes=%+v", changes)
	}
	if changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

// TestGoogleWorkspace_DeltaSync_PaginationAggregates verifies
// that DeltaSync walks the Admin SDK's nextPageToken chain and
// returns every changed principal in a single call. Without the
// inner pagination loop, only the first 200 results would be
// emitted and the rest would be silently dropped because the
// connector advances the cursor to time.Now() on success.
func TestGoogleWorkspace_DeltaSync_PaginationAggregates(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var page1, page2 int
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		// Connect()'s cheap auth probe also lands here with
		// maxResults=1; ignore it so the page counters only
		// track the DeltaSync delta-path requests.
		if r.URL.Query().Get("maxResults") == "1" {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			page1++
			_, _ = io.WriteString(w, `{"users":[
				{"id":"U1","primaryEmail":"alice@x"},
				{"id":"U2","primaryEmail":"bob@x"}
			],"nextPageToken":"p2"}`)
		case "p2":
			page2++
			_, _ = io.WriteString(w, `{"users":[
				{"id":"U3","primaryEmail":"carol@x"},
				{"id":"U4","primaryEmail":"dan@x","suspended":true}
			]}`)
		default:
			t.Fatalf("unexpected pageToken=%q", r.URL.Query().Get("pageToken"))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if page1 != 1 || page2 != 1 {
		t.Fatalf("expected exactly one hit on each page, got page1=%d page2=%d", page1, page2)
	}
	if len(changes) != 4 {
		t.Fatalf("expected 4 aggregated changes across 2 pages, got %d: %+v", len(changes), changes)
	}
	got := []string{changes[0].Ref.ID, changes[1].Ref.ID, changes[2].Ref.ID, changes[3].Ref.ID}
	want := []string{"U1", "U2", "U3", "U4"}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("change[%d].ID=%q want %q (full=%v)", i, got[i], want[i], got)
		}
	}
	if changes[3].Kind != connector.ChangeDeleted {
		t.Fatalf("expected U4 (suspended) to map to ChangeDeleted, got %v", changes[3].Kind)
	}
	if cur == "" {
		t.Fatalf("expected non-empty cursor after pagination completes")
	}
}

func TestGoogleWorkspace_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestGoogleWorkspace_DeltaSync_403QuotaExceededMappedToRateLimited
// pins that an HTTP 403 carrying the canonical Admin SDK
// `userRateLimitExceeded` reason is wrapped as
// connector.ErrRateLimited (and therefore picked up by the
// adaptive rate limiter). This regresses Devin Review's
// Round-17 finding on google_workspace.go:426 — Google's
// primary quota signal is 403 + reason, not 429.
func TestGoogleWorkspace_DeltaSync_403QuotaExceededMappedToRateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// Bootstrap auth probe (maxResults=1) must succeed.
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{
			"error": {
				"code": 403,
				"message": "Quota exceeded",
				"errors": [{"reason": "userRateLimitExceeded", "domain": "usageLimits", "message": "Quota exceeded"}]
			}
		}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("403 userRateLimitExceeded must wrap as ErrRateLimited; got %v", err)
	}
}

// TestGoogleWorkspace_DeltaSync_403ForbiddenNotRateLimited
// pins the negative branch: a genuine HTTP 403 (e.g. revoked
// OAuth grant or missing scope) must surface as a hard
// failure, not as a rate-limit signal. Otherwise the adaptive
// limiter would silently back off forever on a permission
// error that the user needs to fix.
func TestGoogleWorkspace_DeltaSync_403ForbiddenNotRateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"users":[]}`)

			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{
			"error": {
				"code": 403,
				"message": "Insufficient Permission",
				"errors": [{"reason": "insufficientPermissions", "domain": "global", "message": "Insufficient Permission"}]
			}
		}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := googleworkspace.New(googleworkspace.WithBaseURL(srv.URL), googleworkspace.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "2024-01-01T00:00:00Z")
	if err == nil {
		t.Fatal("403 insufficientPermissions must surface as an error")
	}
	if errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("403 insufficientPermissions must NOT wrap as ErrRateLimited; got %v", err)
	}
}

func TestGoogleWorkspace_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := googleworkspace.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
