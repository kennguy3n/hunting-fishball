package entraid_test

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
	entraid "github.com/kennguy3n/hunting-fishball/internal/connector/entra_id"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(entraid.Credentials{AccessToken: "eyJ0.tok"})

	return b
}

func TestEntraID_Validate(t *testing.T) {
	t.Parallel()
	c := entraid.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"missing tenant", connector.ConnectorConfig{SourceID: "s", Credentials: validCreds(t)}, false},
		{"missing source", connector.ConnectorConfig{TenantID: "t", Credentials: validCreds(t)}, false},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
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

func TestEntraID_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestEntraID_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := entraid.New()
	ns, err := c.ListNamespaces(context.Background(), nil)
	if err != nil || len(ns) != 2 || ns[0].ID != "users" || ns[1].ID != "groups" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestEntraID_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	var nextURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skiptoken") == "n1" {
			_, _ = io.WriteString(w, `{"value":[{"id":"U2","displayName":"Bob"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"value":[{"id":"U1","displayName":"Alice"}],"@odata.nextLink":"`+nextURL+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	nextURL = srv.URL + "/users?$skiptoken=n1"
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "U1" || ids[1] != "U2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestEntraID_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "users"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestEntraID_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","displayName":"Alice","userPrincipalName":"alice@acme.com","mail":"alice@acme.com","createdDateTime":"2024-01-01T00:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "users", ID: "U1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Alice" || doc.Author != "alice@acme.com" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "alice@acme.com") {
		t.Fatalf("body=%q", body)
	}
}

func TestEntraID_DeltaSync_BootstrapReturnsDeltaLink(t *testing.T) {
	t.Parallel()
	var initialDelta string
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users/delta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+initialDelta+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	initialDelta = srv.URL + "/users/delta?$deltatoken=NEW"
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap should not emit changes; got %d", len(changes))
	}
	if cur != initialDelta {
		t.Fatalf("cursor=%q want %q", cur, initialDelta)
	}
}

func TestEntraID_DeltaSync_RemovedAndDisabledMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users/delta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[
			{"id":"U1","displayName":"Alice","accountEnabled":true},
			{"id":"U2","displayName":"Bob","accountEnabled":false},
			{"id":"U3","@removed":{}}
		],"@odata.deltaLink":"http://example/users/delta?$deltatoken=Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, srv.URL+"/users/delta?$deltatoken=PREV")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes=%+v", changes)
	}
	want := []connector.ChangeKind{connector.ChangeUpserted, connector.ChangeDeleted, connector.ChangeDeleted}
	for i, ch := range changes {
		if ch.Kind != want[i] {
			t.Fatalf("changes[%d]=%v want %v", i, ch.Kind, want[i])
		}
	}
}

func TestEntraID_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users/delta", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("bootstrap expected ErrRateLimited, got %v", err)
	}
	_, _, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, srv.URL+"/users/delta?$deltatoken=X")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("steady expected ErrRateLimited, got %v", err)
	}
}

func TestEntraID_DeltaSync_PaginationAggregates(t *testing.T) {
	t.Parallel()
	var (
		nextLink  string
		deltaLink string
		hits      [2]int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/users/delta", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skiptoken") == "P2" {
			hits[1]++
			_, _ = io.WriteString(w, `{"value":[
				{"id":"U3","accountEnabled":true},
				{"id":"U4","@removed":{}}
			],"@odata.deltaLink":"`+deltaLink+`"}`)

			return
		}
		hits[0]++
		_, _ = io.WriteString(w, `{"value":[
			{"id":"U1","accountEnabled":true},
			{"id":"U2","accountEnabled":false}
		],"@odata.nextLink":"`+nextLink+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	nextLink = srv.URL + "/users/delta?$skiptoken=P2"
	deltaLink = srv.URL + "/users/delta?$deltatoken=NEW"
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, srv.URL+"/users/delta?$deltatoken=PREV")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if hits[0] != 1 || hits[1] != 1 {
		t.Fatalf("page hits=%v want [1 1]", hits)
	}
	if len(changes) != 4 {
		t.Fatalf("changes=%d want 4 (got %+v)", len(changes), changes)
	}
	want := []connector.ChangeKind{
		connector.ChangeUpserted, // U1
		connector.ChangeDeleted,  // U2 disabled
		connector.ChangeUpserted, // U3 page 2
		connector.ChangeDeleted,  // U4 @removed
	}
	for i, ch := range changes {
		if ch.Kind != want[i] {
			t.Fatalf("changes[%d]=%v want %v (id=%s)", i, ch.Kind, want[i], ch.Ref.ID)
		}
	}
	if cur != deltaLink {
		t.Fatalf("cursor=%q want %q", cur, deltaLink)
	}
}

func TestEntraID_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := entraid.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
