package personio_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/personio"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(personio.Credentials{ClientID: "cid", ClientSecret: "sec"})

	return b
}

func authHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"token":"tok"}}`)
	}
}

func TestPersonio_Validate(t *testing.T) {
	t.Parallel()
	c := personio.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"missing client_secret", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"client_id":"x"}`)}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
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

func TestPersonio_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestPersonio_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := personio.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 1 || ns[0].ID != "employees" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestPersonio_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees", func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		switch off {
		case "0":
			_, _ = io.WriteString(w, `{"data":[{"attributes":{"id":{"value":1}}}]}`)
		case "1":
			_, _ = io.WriteString(w, `{"data":[{"attributes":{"id":{"value":2}}}]}`)
		default:
			_, _ = io.WriteString(w, `{"data":[]}`)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "employees"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestPersonio_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "employees"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestPersonio_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"attributes":{"first_name":{"value":"Alice"},"last_name":{"value":"Lee"},"email":{"value":"alice@x"},"hire_date":{"value":"2024-01-01T00:00:00Z"}}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "employees", ID: "1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Alice Lee" || doc.Author != "alice@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Alice") {
		t.Fatalf("body=%q", body)
	}
}

func TestPersonio_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("bootstrap should be empty + cursor populated; cur=%q changes=%d", cur, len(changes))
	}
}

func TestPersonio_DeltaSync_InactiveMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("updated_from") == "" {
			_, _ = io.WriteString(w, `{"data":[]}`)

			return
		}
		_, _ = io.WriteString(w, `{"data":[
			{"attributes":{"id":{"value":1},"status":{"value":"active"}}},
			{"attributes":{"id":{"value":2},"status":{"value":"inactive"}}}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 || changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestPersonio_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", authHandler())
	mux.HandleFunc("/company/employees", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := personio.New(personio.WithBaseURL(srv.URL), personio.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestPersonio_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := personio.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
