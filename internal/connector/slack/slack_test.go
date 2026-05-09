package slack_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
)

func newSlackServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv
}

func newSlackConnector(srv *httptest.Server) *slack.Connector {
	return slack.New(slack.WithBaseURL(srv.URL), slack.WithHTTPClient(srv.Client()))
}

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(slack.Credentials{BotToken: "xoxb-test"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestSlack_Validate(t *testing.T) {
	t.Parallel()

	s := slack.New()
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
			err := s.Validate(context.Background(), tc.cfg)
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

func TestSlack_Connect(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no token", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1"}`))
	})
	srv := newSlackServer(t, mux)
	s := newSlackConnector(srv)

	conn, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestSlack_Connect_AuthFails(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	srv := newSlackServer(t, mux)
	s := newSlackConnector(srv)

	if _, err := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestSlack_ListNamespaces(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1"}`))
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"channels":[
			{"id":"C1","name":"general","is_private":false},
			{"id":"C2","name":"engineering","is_private":true}
		]}`))
	})
	srv := newSlackServer(t, mux)
	s := newSlackConnector(srv)

	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := s.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 2 {
		t.Fatalf("count: %d", len(ns))
	}
	if ns[1].Kind != "private_channel" {
		t.Fatalf("kind: %q", ns[1].Kind)
	}
}

func TestSlack_ListDocuments(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1"}`))
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"messages":[
			{"ts":"1700000000.000100","user":"U1"},
			{"ts":"1700000001.000100","user":"U2"}
		],"has_more":false}`))
	})
	srv := newSlackServer(t, mux)
	s := newSlackConnector(srv)

	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := s.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
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

func TestSlack_FetchDocument(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1"}`))
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"messages":[{"ts":"1700000000.000100","user":"U1","text":"hello world"}]}`))
	})
	srv := newSlackServer(t, mux)
	s := newSlackConnector(srv)

	conn, _ := s.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := s.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "C1", ID: "1700000000.000100"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello world" {
		t.Fatalf("body: %q", body)
	}
	if doc.Metadata["channel_id"] != "C1" {
		t.Fatalf("metadata: %+v", doc.Metadata)
	}
}

func TestSlack_HandleWebhook_MessageEvent(t *testing.T) {
	t.Parallel()

	s := slack.New()
	payload := []byte(`{
		"type":"event_callback",
		"event": {"type":"message","channel":"C1","ts":"1700000000.000100","user":"U1","text":"hi"}
	}`)
	changes, err := s.HandleWebhook(context.Background(), payload)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes: %d", len(changes))
	}
	if changes[0].Kind != connector.ChangeUpserted {
		t.Fatalf("kind: %v", changes[0].Kind)
	}
	if changes[0].Ref.NamespaceID != "C1" {
		t.Fatalf("ns: %q", changes[0].Ref.NamespaceID)
	}
}

func TestSlack_HandleWebhook_DeletedEvent(t *testing.T) {
	t.Parallel()

	s := slack.New()
	payload := []byte(`{
		"type":"event_callback",
		"event": {"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"1700000000.000100"}
	}`)
	changes, err := s.HandleWebhook(context.Background(), payload)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != connector.ChangeDeleted {
		t.Fatalf("changes: %+v", changes)
	}
}

func TestSlack_HandleWebhook_URLVerification(t *testing.T) {
	t.Parallel()

	s := slack.New()
	payload := []byte(`{"type":"url_verification","challenge":"abc"}`)
	changes, err := s.HandleWebhook(context.Background(), payload)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("expected no changes, got %d", len(changes))
	}
}

func TestSlack_HandleWebhook_Empty(t *testing.T) {
	t.Parallel()

	s := slack.New()
	if _, err := s.HandleWebhook(context.Background(), nil); err == nil {
		t.Fatal("expected error on empty payload")
	}
}

func TestSlack_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	s := slack.New()
	if _, err := s.Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestSlack_WebhookPath(t *testing.T) {
	t.Parallel()

	s := slack.New()
	if got := s.WebhookPath(); got != "/slack" {
		t.Fatalf("WebhookPath: %q", got)
	}
}

func TestSlack_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector("slack"); err != nil {
		t.Fatalf("connector not registered: %v", err)
	}
}
