package quip_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/quip"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(quip.Credentials{AccessToken: "tok"})

	return b
}

func TestQuip_Validate(t *testing.T) {
	t.Parallel()
	c := quip.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
		{"no tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"no source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds(t)}, false},
		{"no creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
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

func TestQuip_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestQuip_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/1/users/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"u1"}`)
	})
	mux.HandleFunc("/1/threads/recent", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"abc":{"thread":{"id":"abc","title":"Spec","author_id":"u1","updated_usec":1717200000000000,"created_usec":1717100000000000},"html":"<p>hi</p>"}}`)
	})
	mux.HandleFunc("/1/threads/abc", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"thread":{"id":"abc","title":"Spec","author_id":"u1","updated_usec":1717200000000000,"created_usec":1717100000000000},"html":"<p>hi</p>"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := c.ListNamespaces(context.Background(), conn)
	if len(ns) != 1 || ns[0].ID != "threads" {
		t.Fatalf("ns=%v", ns)
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
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "threads", ID: "abc"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hi") {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
	if err := c.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestQuip_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/1/users/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "threads"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestQuip_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/1/users/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/1/threads/recent", func(w http.ResponseWriter, _ *http.Request) {
		// One thread newer than the cursor + one older that must
		// be filtered out.
		_, _ = io.WriteString(w, `{"a":{"thread":{"id":"new","updated_usec":1717200000000000}},"b":{"thread":{"id":"old","updated_usec":1700000000000000}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "threads"}, "2024-05-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "new" {
		t.Fatalf("changes=%v", changes)
	}
	if cur == "" {
		t.Fatalf("cur empty")
	}
}

func TestQuip_RateLimited_OnList(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/1/users/current", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/1/threads/recent", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := quip.New(quip.WithBaseURL(srv.URL), quip.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "threads"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}
