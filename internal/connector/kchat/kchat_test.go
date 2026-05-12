package kchat_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
)

func newKChatServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func newKChatConnector(srv *httptest.Server) *kchat.Connector {
	return kchat.New(kchat.WithBaseURL(srv.URL), kchat.WithHTTPClient(srv.Client()))
}

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(kchat.Credentials{APIToken: "kch-test"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestKChat_Validate(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"missing source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds(t)}, false},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := k.Validate(context.Background(), tc.cfg)
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

func TestKChat_Connect(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no token", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, err := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestKChat_Connect_AuthFails(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	if _, err := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestKChat_ListNamespaces(t *testing.T) {
	t.Parallel()

	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels.list", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"channels":[{"id":"C1","name":"general"},{"id":"C2","name":"eng","is_private":true}],"next_cursor":"NEXT"}`))

			return
		}
		_, _ = w.Write([]byte(`{"channels":[{"id":"C3","name":"random"}]}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := k.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 3 {
		t.Fatalf("count: %d", len(ns))
	}
	if calls != 2 {
		t.Fatalf("expected pagination call count 2, got %d", calls)
	}
	if ns[1].Kind != "private_channel" {
		t.Fatalf("kind: %q", ns[1].Kind)
	}
}

func TestKChat_ListDocuments(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[{"id":"m1","updated_at":"2024-01-01T00:00:00Z"},{"id":"m2","updated_at":"2024-01-02T00:00:00Z"}]}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := k.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
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
		t.Fatalf("ids: %+v", ids)
	}
}

func TestKChat_ListDocuments_EmptyPage(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[]}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := k.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
		t.Fatal("expected no docs")
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
}

func TestKChat_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels.history", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate-limited", http.StatusTooManyRequests)
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := k.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if it.Err() == nil || errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("expected non-EOP error, got %v", it.Err())
	}
	if !strings.Contains(it.Err().Error(), "429") && !strings.Contains(it.Err().Error(), "rate") {
		t.Fatalf("error: %v", it.Err())
	}
}

func TestKChat_FetchDocument(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/messages.get", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m1","author":"alice","text":"hi world","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z"}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := k.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "C1", ID: "m1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hi world" {
		t.Fatalf("body: %q", body)
	}
	if doc.Metadata["channel_id"] != "C1" {
		t.Fatalf("metadata: %+v", doc.Metadata)
	}
}

func TestKChat_DeltaSync(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels.changes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"cursor":"C-NEW","changes":[]}`))

			return
		}
		_, _ = w.Write([]byte(`{"cursor":"C-NEW2","changes":[{"kind":"upserted","id":"m1","updated_at":"2024-01-02T00:00:00Z"},{"kind":"deleted","id":"m0"}]}`))
	})
	srv := newKChatServer(t, mux)
	k := newKChatConnector(srv)

	conn, _ := k.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := k.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if cur != "C-NEW" || len(changes) != 0 {
		t.Fatalf("initial sync expected empty changes + new cursor, got %v %q", changes, cur)
	}
	changes, cur, err = k.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if cur != "C-NEW2" || len(changes) != 2 || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("follow sync: %v %q", changes, cur)
	}
}

func TestKChat_HandleWebhook_MessageEvent(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	payload := []byte(`{"type":"message.created","channel":"C1","message":{"id":"m1","updated_at":"2024-01-01T00:00:00Z"}}`)
	changes, err := k.HandleWebhook(context.Background(), payload)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != connector.ChangeUpserted || changes[0].Ref.ID != "m1" {
		t.Fatalf("changes: %+v", changes)
	}
}

func TestKChat_HandleWebhook_DeletedEvent(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	payload := []byte(`{"type":"message.deleted","channel":"C1","message":{"id":"m1"}}`)
	changes, err := k.HandleWebhook(context.Background(), payload)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != connector.ChangeDeleted {
		t.Fatalf("changes: %+v", changes)
	}
}

func TestKChat_HandleWebhook_URLVerification(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	changes, err := k.HandleWebhook(context.Background(), []byte(`{"type":"url_verification","challenge":"abc"}`))
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %d", len(changes))
	}
}

func TestKChat_HandleWebhook_Empty(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	if _, err := k.HandleWebhook(context.Background(), nil); err == nil {
		t.Fatal("expected error on empty payload")
	}
}

func TestKChat_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	k := kchat.New()
	if _, err := k.Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestKChat_WebhookPath(t *testing.T) {
	t.Parallel()

	if got := kchat.New().WebhookPath(); got != "/kchat" {
		t.Fatalf("WebhookPath: %q", got)
	}
}

func TestKChat_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector(kchat.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
