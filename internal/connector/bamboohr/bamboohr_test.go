package bamboohr_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/bamboohr"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(bamboohr.Credentials{APIKey: "key", Subdomain: "acme"})

	return b
}

func TestBambooHR_Validate(t *testing.T) {
	t.Parallel()
	c := bamboohr.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing api_key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"subdomain":"acme"}`)}, false},
		{"missing subdomain", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"api_key":"k"}`)}, false},
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

func TestBambooHR_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestBambooHR_BasicAuthHeader(t *testing.T) {
	t.Parallel()
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = io.WriteString(w, `{"employees":[]}`)
	}))
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if gotUser != "key" || gotPass != "x" {
		t.Fatalf("basic auth user=%q pass=%q", gotUser, gotPass)
	}
}

func TestBambooHR_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := bamboohr.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 1 || ns[0].ID != "employees" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestBambooHR_ListDocuments(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[{"id":"1","displayName":"Alice"},{"id":"2","displayName":"Bob"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
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

func TestBambooHR_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = io.WriteString(w, `{"employees":[]}`)

			return
		}
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "employees"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestBambooHR_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[]}`)
	})
	mux.HandleFunc("/v1/employees/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"1","displayName":"Alice","workEmail":"alice@x","jobTitle":"Engineer","hireDate":"2024-01-15"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "employees", ID: "1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Alice" || doc.Author != "alice@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Engineer") {
		t.Fatalf("body=%q", body)
	}
}

func TestBambooHR_DeltaSync_BootstrapReturnsNow(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 || cur == "" {
		t.Fatalf("bootstrap should return empty changes + cursor; cur=%q changes=%d", cur, len(changes))
	}
}

func TestBambooHR_DeltaSync_DeletedActionMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[]}`)
	})
	mux.HandleFunc("/v1/employees/changed", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":{
			"1":{"action":"Updated","lastChanged":"2024-06-01T00:00:00Z"},
			"2":{"action":"Deleted","lastChanged":"2024-06-02T00:00:00Z"}
		}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes=%+v", changes)
	}
	kinds := map[string]connector.ChangeKind{}
	for _, ch := range changes {
		kinds[ch.Ref.ID] = ch.Kind
	}
	if kinds["1"] != connector.ChangeUpserted || kinds["2"] != connector.ChangeDeleted {
		t.Fatalf("kinds=%+v", kinds)
	}
}

func TestBambooHR_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[]}`)
	})
	mux.HandleFunc("/v1/employees/changed", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestBambooHR_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := bamboohr.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
