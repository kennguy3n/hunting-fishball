package jira_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/jira"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *jira.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	return srv, jira.New(jira.WithHTTPClient(srv.Client()), jira.WithBaseURL(srv.URL))
}

func creds(site string) []byte {
	return []byte(fmt.Sprintf(`{"email":"u@x.com","api_token":"tok","site_url":%q}`, site))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := jira.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			_, _ = io.WriteString(w, `{"accountId":"u"}`)
		case r.URL.Path == "/rest/api/3/project":
			_, _ = io.WriteString(w, `[{"id":"1","key":"ENG","name":"Engineering"}]`)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			_, _ = io.WriteString(w, `{"startAt":0,"maxResults":100,"total":1,"issues":[{"key":"ENG-1","fields":{"updated":"2024-01-02T00:00:00.000+0000"}}]}`)
		case r.URL.Path == "/rest/api/3/issue/ENG-1":
			_, _ = io.WriteString(w, `{"key":"ENG-1","fields":{"summary":"hello","created":"2024-01-01T00:00:00.000+0000","updated":"2024-01-02T00:00:00.000+0000"}}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: creds(srv.URL),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].ID != "ENG" {
		t.Fatalf("ns: %+v err=%v", ns, err)
	}
	it, _ := c.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	if !it.Next(context.Background()) {
		t.Fatalf("next: %v", it.Err())
	}
	doc, err := c.FetchDocument(context.Background(), conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "hello" {
		t.Fatalf("title: %q", doc.Title)
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/myself":
			_, _ = io.WriteString(w, `{"accountId":"u"}`)
		case strings.HasPrefix(r.URL.Path, "/rest/api/3/search"):
			_, _ = io.WriteString(w, `{"issues":[{"key":"ENG-1","fields":{"updated":"2025-01-02T00:00:00.000+0000"}}]}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: creds(srv.URL),
	})

	_, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ENG"}, "")
	if err != nil {
		t.Fatalf("delta init: %v", err)
	}
	if cur == "" {
		t.Fatal("empty cursor")
	}
	ch, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ENG"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 1 {
		t.Fatalf("changes: %d", len(ch))
	}
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	c := jira.New()
	ch, err := c.HandleWebhook(context.Background(), []byte(`{"webhookEvent":"jira:issue_updated","issue":{"key":"ENG-1"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) != 1 || ch[0].Ref.ID != "ENG-1" || ch[0].Kind != connector.ChangeUpserted {
		t.Fatalf("unexpected: %+v", ch)
	}
	delCh, err := c.HandleWebhook(context.Background(), []byte(`{"webhookEvent":"jira:issue_deleted","issue":{"key":"ENG-2"}}`))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if delCh[0].Kind != connector.ChangeDeleted {
		t.Fatalf("kind: %v", delCh[0])
	}
	if c.WebhookPath() == "" {
		t.Fatalf("empty path")
	}
}
