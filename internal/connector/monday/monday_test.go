package monday_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/monday"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(monday.Credentials{APIToken: "eyJ-test"})

	return b
}

func TestMonday_Validate(t *testing.T) {
	t.Parallel()
	m := monday.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing token", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
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

func TestMonday_Connect_RateLimited_HTTP429(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	_, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestMonday_Connect_RateLimited_GraphQLEnvelope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Complexity budget exhausted","extensions":{"code":"ComplexityException","status_code":429}}]}`)
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	_, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited from GraphQL envelope, got %v", err)
	}
}

func TestMonday_ListNamespaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "me{id}"):
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"U1"}}}`)
		case strings.Contains(s, "boards(limit"):
			_, _ = io.WriteString(w, `{"data":{"boards":[{"id":"B1","name":"Roadmap"}]}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	conn, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := m.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].Kind != "board" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestMonday_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "me{id}"):
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"U1"}}}`)
		case strings.Contains(s, "items_page(limit:100)"):
			_, _ = io.WriteString(w, `{"data":{"boards":[{"items_page":{"cursor":"C2","items":[{"id":"I1","updated_at":"2024-01-01T00:00:00Z"}]}}]}}`)
		case strings.Contains(s, "next_items_page"):
			if first {
				first = false
				_, _ = io.WriteString(w, `{"data":{"next_items_page":{"cursor":"","items":[{"id":"I2","updated_at":"2024-01-02T00:00:00Z"}]}}}`)

				return
			}
			_, _ = io.WriteString(w, `{"data":{"next_items_page":{"cursor":"","items":[]}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := m.ListDocuments(context.Background(), conn, connector.Namespace{ID: "B1"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
}

func TestMonday_FetchDocument(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "me{id}"):
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"U1"}}}`)
		case strings.Contains(s, "items(ids:"):
			_, _ = io.WriteString(w, `{"data":{"items":[{"id":"I1","name":"Feature","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-02T00:00:00Z","creator":{"name":"alice"}}]}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := m.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "B1", ID: "I1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Feature" || doc.Author != "alice" {
		t.Fatalf("doc=%+v", doc)
	}
}

func TestMonday_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		switch {
		case strings.Contains(s, "me{id}"):
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"U1"}}}`)
		case strings.Contains(s, "direction:desc"):
			_, _ = io.WriteString(w, `{"data":{"boards":[{"items_page":{"items":[{"updated_at":"2024-01-05T00:00:00Z"}]}}]}}`)
		case strings.Contains(s, "operator:greater_than"):
			_, _ = io.WriteString(w, `{"data":{"boards":[{"items_page":{"cursor":"","items":[{"id":"I2","updated_at":"2024-01-10T00:00:00Z"}]}}]}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	conn, _ := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "B1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial should not backfill: %v", changes)
	}
	if cur != "2024-01-05T00:00:00Z" {
		t.Fatalf("cur=%q", cur)
	}
	changes, newCur, err := m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "B1"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "I2" || newCur != "2024-01-10T00:00:00Z" {
		t.Fatalf("changes=%v newCur=%q", changes, newCur)
	}
}

func TestMonday_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		if strings.Contains(s, "me{id}") {
			_, _ = io.WriteString(w, `{"data":{"me":{"id":"U1"}}}`)

			return
		}
		calls++
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	m := monday.New(monday.WithBaseURL(srv.URL), monday.WithHTTPClient(srv.Client()))
	conn, err := m.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, _, err = m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "B1"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = m.DeltaSync(context.Background(), conn, connector.Namespace{ID: "B1"}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 GraphQL calls, got %d", calls)
	}
}

func TestMonday_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	m := monday.New()
	_, err := m.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
