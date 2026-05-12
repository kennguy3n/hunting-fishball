package discord_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/discord"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(discord.Credentials{BotToken: "bot-test"})

	return b
}

func newDiscordServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

func TestDiscord_Validate(t *testing.T) {
	t.Parallel()
	d := discord.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := d.Validate(context.Background(), tc.cfg)
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

func TestDiscord_Connect(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bot ") {
			http.Error(w, "no", http.StatusUnauthorized)

			return
		}
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, err := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestDiscord_Connect_AuthFails(t *testing.T) {
	t.Parallel()
	srv := newDiscordServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	if _, err := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDiscord_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me/guilds", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"G1","name":"Guild1"}]`))
	})
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/guilds/G1/channels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"C1","name":"general","type":0},{"id":"V1","name":"voice","type":2}]`))
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := d.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 1 || ns[0].ID != "C1" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestDiscord_ListDocuments(t *testing.T) {
	t.Parallel()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels/C1/messages", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`[{"id":"M2","timestamp":"2024-01-02T00:00:00Z"},{"id":"M1","timestamp":"2024-01-01T00:00:00Z"}]`))

			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := d.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
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

func TestDiscord_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels/C1/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := d.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if it.Err() == nil || errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("expected non-EOP, got %v", it.Err())
	}
}

func TestDiscord_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels/C1/messages/M1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"M1","content":"hi","timestamp":"2024-01-01T00:00:00Z","author":{"username":"alice"}}`))
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := d.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "C1", ID: "M1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	b, _ := io.ReadAll(doc.Content)
	if string(b) != "hi" {
		t.Fatalf("body: %q", b)
	}
}

func TestDiscord_DeltaSync(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels/C1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			_, _ = w.Write([]byte(`[{"id":"M2","timestamp":"2024-01-02T00:00:00Z"}]`))

			return
		}
		_, _ = w.Write([]byte(`[{"id":"M3","timestamp":"2024-01-03T00:00:00Z"}]`))
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := d.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "M2" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = d.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "M0")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || cur != "M3" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

// TestDiscord_DeltaSync_RateLimited locks in that a 429 during a
// delta sync surfaces as connector.ErrRateLimited so the adaptive
// rate limiter can react — matching the ListDocuments iterator.
func TestDiscord_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/@me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"U1"}`))
	})
	mux.HandleFunc("/channels/C1/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := newDiscordServer(t, mux)
	d := discord.New(discord.WithBaseURL(srv.URL), discord.WithHTTPClient(srv.Client()))
	conn, _ := d.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := d.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestDiscord_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()
	if _, err := discord.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestDiscord_Registered(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(discord.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
