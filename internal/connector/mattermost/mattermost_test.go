package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
)

func validCreds(t *testing.T, base string) []byte {
	t.Helper()
	b, _ := json.Marshal(mattermost.Credentials{AccessToken: "tok", BaseURL: base})

	return b
}

func TestMattermost_Validate(t *testing.T) {
	t.Parallel()
	m := mattermost.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, "https://mm.example")}, true},
		{"empty", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"base_url":"https://x"}`)}, false},
		{"missing base", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"t"}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := m.Validate(context.Background(), tc.cfg)
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

func TestMattermost_Connect_AuthFail(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	_, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err == nil {
		t.Fatal("expected auth error")
	}
}

func TestMattermost_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	_, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestMattermost_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/users/U1/channels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"C1","name":"general","display_name":"General","type":"O","team_id":"T1"}]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := m.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].Kind != "channel" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestMattermost_ListNamespaces_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/users/U1/channels", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, err := m.ListNamespaces(context.Background(), conn)
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestMattermost_ListDocuments(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/channels/C1/posts", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"order":["P1"],"posts":{"P1":{"id":"P1","update_at":1700000000000}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, err := m.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if len(ids) != 1 || ids[0] != "P1" {
		t.Fatalf("ids=%v err=%v", ids, it.Err())
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v want EndOfPage", it.Err())
	}
}

func TestMattermost_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/channels/C1/posts", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := m.ListDocuments(context.Background(), conn, connector.Namespace{ID: "C1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestMattermost_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/posts/P1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"P1","message":"hello world","user_id":"U2","channel_id":"C1","create_at":1700000000000,"update_at":1700000000000}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := m.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "C1", ID: "P1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Author != "U2" || !strings.Contains(doc.Title, "hello") {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body=%q", body)
	}
}

func TestMattermost_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var since string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/channels/C1/posts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			_, _ = io.WriteString(w, `{"order":["PNEW"],"posts":{"PNEW":{"id":"PNEW","update_at":1700000005000}}}`)

			return
		}
		since = r.URL.Query().Get("since")
		_, _ = io.WriteString(w, `{"order":["P2"],"posts":{"P2":{"id":"P2","update_at":1700000010000}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial should not backfill: %v", changes)
	}
	if cur != "1700000005000" {
		t.Fatalf("cur=%q want 1700000005000", cur)
	}
	changes, newCur, err := m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if since != cur {
		t.Fatalf("since=%q want %q", since, cur)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "P2" {
		t.Fatalf("changes=%v", changes)
	}
	got, _ := strconv.ParseInt(newCur, 10, 64)
	if got != 1700000010000 {
		t.Fatalf("newCur=%d", got)
	}
}

func TestMattermost_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/api/v4/channels/C1/posts", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := mattermost.New(mattermost.WithBaseURL(srv.URL), mattermost.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	_, _, err := m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "C1"}, "1700000000000")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestMattermost_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	m := mattermost.New(mattermost.WithBaseURL("http://x"))
	_, err := m.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
