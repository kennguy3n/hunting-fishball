package pagerduty_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/pagerduty"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(pagerduty.Credentials{APIKey: "k"})

	return b
}

func TestPagerDuty_Validate(t *testing.T) {
	t.Parallel()
	c := pagerduty.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
		{"no tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"no source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds(t)}, false},
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

func TestPagerDuty_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestPagerDuty_Connect_ServiceTokenFallback(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/abilities", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"abilities":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
	if _, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestPagerDuty_Lifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{}}`)
	})
	mux.HandleFunc("/incidents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"incidents":[{"id":"P1","title":"DB down","summary":"prod db unreachable","last_status_change_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}],"more":false}`)
	})
	mux.HandleFunc("/incidents/P1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"incident":{"id":"P1","title":"DB down","summary":"prod db unreachable","last_status_change_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
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
	if len(ids) != 1 || ids[0] != "P1" {
		t.Fatalf("ids=%v", ids)
	}
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "incidents", ID: "P1"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "prod") {
		t.Fatalf("body=%q", body)
	}
	if _, err := c.Subscribe(context.Background(), conn, ns[0]); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("Subscribe should be ErrNotSupported, got %v", err)
	}
	if err := c.Disconnect(context.Background(), conn); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}

func TestPagerDuty_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incidents"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("changes=%v cur=%q", changes, cur)
	}
}

func TestPagerDuty_DeltaSync_Incremental(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/incidents", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "" {
			http.Error(w, "missing since", http.StatusBadRequest)

			return
		}
		_, _ = io.WriteString(w, `{"incidents":[{"id":"P2","last_status_change_at":"2024-06-02T00:00:00Z"}],"more":false}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incidents"}, "2024-05-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "P2" {
		t.Fatalf("changes=%v", changes)
	}
	if cur == "" {
		t.Fatalf("cur empty")
	}
}

func TestPagerDuty_RateLimited_OnList(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/users/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/incidents", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := pagerduty.New(pagerduty.WithBaseURL(srv.URL), pagerduty.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "incidents"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}
