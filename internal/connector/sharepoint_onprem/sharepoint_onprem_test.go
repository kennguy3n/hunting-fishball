package sharepointonprem_test

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
	sharepointonprem "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint_onprem"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(sharepointonprem.Credentials{BaseURL: "https://sp.test", Username: "u", Password: "p"})

	return b
}

func TestSharePointOnprem_Validate(t *testing.T) {
	t.Parallel()
	c := sharepointonprem.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"no creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"no auth", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_url":"https://sp.test"}`)}, false},
	}
	for _, tc := range cases {
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

func TestSharePointOnprem_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestSharePointOnprem_ListLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/_api/web", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Title":"Eng"}`)
	})
	mux.HandleFunc("/_api/web/lists", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"Id":"L1","Title":"Docs"}]}`)
	})
	mux.HandleFunc("/_api/web/lists(guid'L1')/items", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"Id":1,"Modified":"2024-06-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/_api/web/lists(guid'L1')/items(1)", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Title":"Spec","Modified":"2024-06-01T00:00:00Z","Created":"2024-01-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].ID != "L1" {
		t.Fatalf("ns=%+v err=%v", ns, err)
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
	if len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "L1", ID: "1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Spec") {
		t.Fatalf("body=%q", body)
	}
}

func TestSharePointOnprem_ListItems_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/_api/web", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/_api/web/lists", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"Id":"L1","Title":"Docs"}]}`)
	})
	mux.HandleFunc("/_api/web/lists(guid'L1')/items", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "L1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestSharePointOnprem_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/_api/web", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur == "" {
		t.Fatal("bootstrap cursor empty")
	}
}

func TestSharePointOnprem_DeltaSync_StopsAtCursor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/_api/web", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/_api/web/lists(guid'L1')/items", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "Modified+gt+datetime") {
			http.Error(w, "missing filter", http.StatusBadRequest)

			return
		}
		_, _ = io.WriteString(w, `{"value":[{"Id":2,"Modified":"2024-06-02T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "2" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestSharePointOnprem_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/_api/web", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/_api/web/lists(guid'L1')/items", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sharepointonprem.New(sharepointonprem.WithBaseURL(srv.URL), sharepointonprem.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "2024-06-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestSharePointOnprem_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := sharepointonprem.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
