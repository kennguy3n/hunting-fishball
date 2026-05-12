package gmail_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(gmail.Credentials{AccessToken: "ya29-test"})

	return b
}

func TestGmail_Validate(t *testing.T) {
	t.Parallel()
	g := gmail.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := g.Validate(context.Background(), tc.cfg)
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

func TestGmail_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	_, err := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestGmail_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"42"}`)
	})
	mux.HandleFunc("/users/me/labels", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"L1","name":"Work","type":"user"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := g.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 2 {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestGmail_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"42"}`)
	})
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := g.ListDocuments(context.Background(), conn, connector.Namespace{ID: "INBOX"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestGmail_ListDocuments_HappyPath(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"42"}`)
	})
	mux.HandleFunc("/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"messages":[{"id":"M1"},{"id":"M2"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := g.ListDocuments(context.Background(), conn, connector.Namespace{ID: "INBOX"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "M1" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestGmail_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"42"}`)
	})
	mux.HandleFunc("/users/me/messages/M1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","internalDate":"1700000000000","snippet":"Hello","historyId":"100","payload":{"headers":[{"name":"Subject","value":"Hi"},{"name":"From","value":"bob@x"}]}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := g.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "INBOX", ID: "M1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Hi" || doc.Author != "bob@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Hello") {
		t.Fatalf("body=%q", body)
	}
}

func TestGmail_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var gotStart string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"500"}`)
	})
	mux.HandleFunc("/users/me/history", func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("startHistoryId")
		_, _ = io.WriteString(w, `{"history":[{"id":"501","messagesAdded":[{"message":{"id":"M9"}}]}],"historyId":"600"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "500" {
		t.Fatalf("initial cur=%q changes=%v", cur, changes)
	}
	changes, newCur, err := g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if gotStart != cur {
		t.Fatalf("startHistoryId=%q want %q", gotStart, cur)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "M9" || newCur != "600" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

func TestGmail_DeltaSync_Deletes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"500"}`)
	})
	mux.HandleFunc("/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"history":[{"id":"501","messagesDeleted":[{"message":{"id":"M5"}}]}],"historyId":"600"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	conn, _ := g.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "100")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Kind != connector.ChangeDeleted || changes[0].Ref.ID != "M5" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestGmail_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
	// Connect itself succeeds against a different server — but we
	// drive DeltaSync against the rate-limited server using a hand
	// rolled conn via a real Connect flow. To do that we'll set up
	// connect against a 200 profile server first.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"historyId":"1"}`)
	}))
	defer srv2.Close()
	conn, err := gmail.New(gmail.WithBaseURL(srv2.URL), gmail.WithHTTPClient(srv2.Client())).Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err = g.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	// Steady path: replace profile route with history handler.
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv3 := httptest.NewServer(mux2)
	defer srv3.Close()
	g3 := gmail.New(gmail.WithBaseURL(srv3.URL), gmail.WithHTTPClient(srv3.Client()))
	_, _, err = g3.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "100")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestGmail_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	g := gmail.New()
	_, err := g.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
