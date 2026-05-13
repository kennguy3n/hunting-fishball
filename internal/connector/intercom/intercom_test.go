package intercom_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/intercom"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(intercom.Credentials{AccessToken: "oat"})

	return b
}

func TestIntercom_Validate(t *testing.T) {
	t.Parallel()
	c := intercom.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
		{"no tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
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

func TestIntercom_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestIntercom_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"app":{"id_code":"x"}}`)
	})
	mux.HandleFunc("/conversations", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"conversations":[{"id":"C1","updated_at":1717286400,"created_at":1717200000}]}`)
	})
	mux.HandleFunc("/conversations/C1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"C1","title":"t","updated_at":1717286400,"created_at":1717200000}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 2 {
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
	if len(ids) != 1 || ids[0] != "C1" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "conversations", ID: "C1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Ref.ID != "C1" {
		t.Fatalf("doc=%+v", doc.Ref)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
}

func TestIntercom_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "conversations"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestIntercom_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/conversations/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)

			return
		}
		_, _ = io.WriteString(w, `{"conversations":[{"id":"C9","updated_at":1717286400}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "conversations"}, "1717200000")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || cur != "1717286400" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestIntercom_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/conversations/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "conversations"}, "1717200000")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestIntercom_ListDocuments_FollowsStartingAfter verifies the
// Round-22 pagination fix: ListDocuments must thread the
// `pages.next.starting_after` cursor until Intercom stops
// echoing it back.
func TestIntercom_ListDocuments_FollowsStartingAfter(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/conversations", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("starting_after") {
		case "":
			_, _ = io.WriteString(w, `{"conversations":[{"id":"c1","updated_at":1717200000}],"pages":{"next":{"starting_after":"cur-2"}}}`)
		case "cur-2":
			_, _ = io.WriteString(w, `{"conversations":[{"id":"c2","updated_at":1717200001}]}`)
		default:
			http.Error(w, "unexpected starting_after", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "conversations"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Fatalf("expected 2 IDs across 2 pages, got %v", ids)
	}
}

// TestIntercom_DeltaSync_ArticlesUsesListEndpoint asserts the
// Round-22 Devin Review fix: DeltaSync against the `articles`
// namespace must walk GET /articles (with client-side updated_at
// filtering) rather than the non-existent `/articles/search`
// endpoint that the original implementation hardcoded.
func TestIntercom_DeltaSync_ArticlesUsesListEndpoint(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/articles/search", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "/articles/search does not exist on Intercom", http.StatusNotFound)
	})
	calls := 0
	mux.HandleFunc("/articles", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "bad method", http.StatusMethodNotAllowed)

			return
		}
		calls++
		switch r.URL.Query().Get("starting_after") {
		case "":
			// Two articles — one stale (before cursor), one fresh.
			_, _ = io.WriteString(w, `{"data":[{"id":"old","updated_at":1717100000},{"id":"a1","updated_at":1717200500}],"pages":{"next":{"starting_after":"page2"}}}`)
		case "page2":
			_, _ = io.WriteString(w, `{"data":[{"id":"a2","updated_at":1717201500}]}`)
		default:
			http.Error(w, "unexpected starting_after", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := intercom.New(intercom.WithBaseURL(srv.URL), intercom.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "articles"}, "1717200000")
	if err != nil {
		t.Fatalf("DeltaSync articles: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 GET /articles calls (initial + starting_after), got %d", calls)
	}
	if len(changes) != 2 || changes[0].Ref.ID != "a1" || changes[1].Ref.ID != "a2" {
		t.Fatalf("changes=%+v (expected only fresh a1 + a2, stale 'old' filtered client-side)", changes)
	}
	if cur != "1717201500" {
		t.Fatalf("cur=%q want 1717201500 (newest updated_at across pages)", cur)
	}
}

func TestIntercom_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(intercom.Name); err != nil {
		t.Fatalf("registry missing intercom: %v", err)
	}
}
