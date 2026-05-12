package gcs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gcs"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(gcs.Credentials{AccessToken: "tok", Bucket: "b1"})

	return b
}

func TestGCS_Validate(t *testing.T) {
	t.Parallel()
	c := gcs.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"bucket":"b"}`)}, false},
		{"no bucket", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"t"}`)}, false},
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

func TestGCS_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGCS_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/b1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"b1"}`)
	})
	mux.HandleFunc("/storage/v1/b/b1/o", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"name":"f.txt","contentType":"text/plain","updated":"2024-06-01T00:00:00Z","etag":"e1"}]}`)
	})
	mux.HandleFunc("/storage/v1/b/b1/o/f.txt", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "media" {
			http.Error(w, "want alt=media", http.StatusBadRequest)

			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 {
		t.Fatalf("ns=%v err=%v", ns, err)
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
	if len(ids) != 1 || ids[0] != "f.txt" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "b1", ID: "f.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello" {
		t.Fatalf("body=%q", body)
	}
}

func TestGCS_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/b1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	fixed := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()), gcs.WithNow(func() time.Time { return fixed }))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "b1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur != "2024-07-01T12:00:00Z" {
		t.Fatalf("cursor=%q", cur)
	}
}

func TestGCS_DeltaSync_StopsAtCursor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/b1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/storage/v1/b/b1/o", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[
			{"name":"old","updated":"2024-06-01T00:00:00Z","etag":"a"},
			{"name":"new","updated":"2024-06-03T00:00:00Z","etag":"b"}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "b1"}, "2024-06-02T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "new" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestGCS_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/v1/b/b1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/storage/v1/b/b1/o", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "b1"}, "2024-06-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGCS_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := gcs.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
