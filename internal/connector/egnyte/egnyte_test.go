package egnyte_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/egnyte"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(egnyte.Credentials{AccessToken: "tok", Domain: "acme"})

	return b
}

func TestEgnyte_Validate(t *testing.T) {
	t.Parallel()
	c := egnyte.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"domain":"acme"}`)}, false},
		{"no domain", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"t"}`)}, false},
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

func TestEgnyte_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := egnyte.New(egnyte.WithBaseURL(srv.URL), egnyte.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestEgnyte_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/pubapi/v1/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"username":"alice"}`)
	})
	mux.HandleFunc("/pubapi/v1/fs/Shared", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"files":[{"entry_id":"e1","name":"spec.md","path":"/Shared/spec.md","last_modified":"Mon, 01 Jul 2024 12:00:00 GMT","checksum":"c1"}]}`)
	})
	mux.HandleFunc("/pubapi/v1/fs-content/Shared/spec.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = io.WriteString(w, "# Spec")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := egnyte.New(egnyte.WithBaseURL(srv.URL), egnyte.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].ID != "/Shared" {
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
	if len(ids) != 1 || ids[0] != "/Shared/spec.md" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "/Shared", ID: "/Shared/spec.md"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "# Spec" {
		t.Fatalf("body=%q", body)
	}
}

func TestEgnyte_DeltaSync_BootstrapReturnsCursor(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/pubapi/v1/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/pubapi/v2/events/cursor", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"latest_event_id":42}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := egnyte.New(egnyte.WithBaseURL(srv.URL), egnyte.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "/Shared"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur != "42" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestEgnyte_DeltaSync_EmitsUpsertAndDelete(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/pubapi/v1/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/pubapi/v2/events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"latest_id":50,
			"events":[
				{"id":43,"type":"file","action":"create","data":{"target_path":"/Shared/a","timestamp":"2024-07-01T12:00:00Z"}},
				{"id":44,"type":"file","action":"delete","data":{"target_path":"/Shared/b","timestamp":"2024-07-01T12:00:00Z"}}
			]
		}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := egnyte.New(egnyte.WithBaseURL(srv.URL), egnyte.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "/Shared"}, "42")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if cur != "50" {
		t.Fatalf("cursor=%q", cur)
	}
	if len(changes) != 2 || changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestEgnyte_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/pubapi/v1/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/pubapi/v2/events/cursor", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := egnyte.New(egnyte.WithBaseURL(srv.URL), egnyte.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "/Shared"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestEgnyte_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := egnyte.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
