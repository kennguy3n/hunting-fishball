package bitbucket_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/bitbucket"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(bitbucket.Credentials{Username: "u", AppPassword: "p", Workspace: "ws", Repo: "r"})

	return b
}

func TestBitbucket_Validate(t *testing.T) {
	t.Parallel()
	c := bitbucket.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no user", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"app_password":"p","workspace":"w","repo":"r"}`)}, false},
		{"no workspace", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"username":"u","app_password":"p","repo":"r"}`)}, false},
		{"no repo", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"username":"u","app_password":"p","workspace":"w"}`)}, false},
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

func TestBitbucket_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestBitbucket_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			http.Error(w, "no auth", http.StatusUnauthorized)

			return
		}
		_, _ = io.WriteString(w, `{"slug":"r"}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"values":[{"id":7,"title":"Bump","updated_on":"2024-06-01T00:00:00Z","created_on":"2024-05-01T00:00:00Z","state":"OPEN","summary":{"raw":"body"}}]}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests/7", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":7,"title":"Bump","updated_on":"2024-06-01T00:00:00Z","created_on":"2024-05-01T00:00:00Z","state":"OPEN","summary":{"raw":"body"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
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
	if len(ids) != 1 || ids[0] != "7" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "ws/r", ID: "7"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "body" {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
}

func TestBitbucket_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ws/r"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestBitbucket_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "q=") {
			_, _ = io.WriteString(w, `{"values":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"values":[{"id":9,"updated_on":"2024-06-02T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ws/r"}, "2024-06-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || cur != "2024-06-02T00:00:00Z" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestBitbucket_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ws/r"}, "2024-06-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

// TestBitbucket_ListDocuments_FollowsNextURL verifies the Round-22
// pagination fix: ListDocuments must follow Bitbucket's `next`
// field across pages instead of stopping after the first response.
func TestBitbucket_ListDocuments_FollowsNextURL(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			_, _ = io.WriteString(w, `{"values":[{"id":1,"title":"a","updated_on":"2024-06-01T00:00:00Z","summary":{"raw":"body1"}}],"next":"`+srvURL+`/2.0/repositories/ws/r/pullrequests?page=2"}`)
		case "2":
			_, _ = io.WriteString(w, `{"values":[{"id":2,"title":"b","updated_on":"2024-06-02T00:00:00Z","summary":{"raw":"body2"}}]}`)
		default:
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "ws/r"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("iter err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("expected 2 IDs across 2 pages, got %v", ids)
	}
}

func TestBitbucket_Registers(t *testing.T) {
	t.Parallel()
	if _, err := connector.GetSourceConnector(bitbucket.Name); err != nil {
		t.Fatalf("registry missing bitbucket: %v", err)
	}
}
