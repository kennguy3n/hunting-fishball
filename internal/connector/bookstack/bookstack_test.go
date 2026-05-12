package bookstack_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/bookstack"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(bookstack.Credentials{BaseURL: "https://wiki", TokenID: "id", TokenSecret: "sec"})

	return b
}

func TestBookstack_Validate(t *testing.T) {
	t.Parallel()
	c := bookstack.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_url":"u"}`)}, false},
		{"no base", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"token_id":"i","token_secret":"s"}`)}, false},
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

func TestBookstack_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestBookstack_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":1,"name":"Spec","updated_at":"2024-07-01T12:00:00Z"}]}`)
	})
	mux.HandleFunc("/api/pages/1", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Token id:sec") {
			http.Error(w, "wrong token", http.StatusUnauthorized)

			return
		}
		_, _ = io.WriteString(w, `{"id":1,"name":"Spec","book_id":2,"chapter_id":3,"html":"<p>hi</p>","updated_at":"2024-07-01T12:00:00Z","created_at":"2024-01-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
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
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "pages", ID: "1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hi") {
		t.Fatalf("body=%q", body)
	}
}

func TestBookstack_DeltaSync_BootstrapReturnsLatest(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("count") == "1" {
			_, _ = io.WriteString(w, `{"data":[{"id":99,"updated_at":"2024-07-01T12:00:00Z"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "pages"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur != "2024-07-01T12:00:00Z" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestBookstack_DeltaSync_StopsAtCursor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[
			{"id":3,"updated_at":"2024-07-03T00:00:00Z"},
			{"id":2,"updated_at":"2024-07-02T00:00:00Z"},
			{"id":1,"updated_at":"2024-07-01T00:00:00Z"}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "pages"}, "2024-07-01T12:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestBookstack_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "pages"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestBookstack_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := bookstack.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
