package clickup_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(clickup.Credentials{APIKey: "pk_test", TeamID: "T1"})

	return b
}

func TestClickUp_Validate(t *testing.T) {
	t.Parallel()
	c := clickup.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"team_id":"T"}`)}, false},
		{"missing team", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_key":"k"}`)}, false},
	} {
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

func TestClickUp_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestClickUp_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/team/T1/space", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"spaces":[{"id":"S1","name":"Eng"}]}`)
	})
	mux.HandleFunc("/space/S1/list", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"lists":[{"id":"L1","name":"Sprint"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 1 || ns[0].ID != "L1" || ns[0].Kind != "list" {
		t.Fatalf("ns=%+v", ns)
	}
}

func TestClickUp_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/list/L1/task", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "L1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestClickUp_ListDocuments_HappyPath(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/list/L1/task", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tasks":[{"id":"T1","date_updated":"1700000000000"}],"last_page":true}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "L1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	count := 0
	for it.Next(context.Background()) {
		count++
	}
	if count != 1 {
		t.Fatalf("got %d", count)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
}

func TestClickUp_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/task/T1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"T1","name":"Bug","description":"Detail","date_created":"1700000000000","date_updated":"1700000000000","creator":{"username":"alice"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "L1", ID: "T1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Bug" || doc.Author != "alice" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Detail") {
		t.Fatalf("body=%q", body)
	}
}

func TestClickUp_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	var gotDateGT string
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/list/L1/task", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reverse") == "true" {
			_, _ = io.WriteString(w, `{"tasks":[{"id":"TNEW","date_updated":"1700000005000"}]}`)

			return
		}
		gotDateGT = r.URL.Query().Get("date_updated_gt")
		_, _ = io.WriteString(w, `{"tasks":[{"id":"T2","date_updated":"1700000010000"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial should not backfill: %v", changes)
	}
	if cur != "1700000005000" {
		t.Fatalf("cur=%q want 1700000005000", cur)
	}
	changes, newCur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if gotDateGT != cur {
		t.Fatalf("date_updated_gt=%q want %q", gotDateGT, cur)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "T2" || newCur != "1700000010000" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

func TestClickUp_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/list/L1/task", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := clickup.New(clickup.WithBaseURL(srv.URL), clickup.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "L1"}, "1700000000000")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestClickUp_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := clickup.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
