package teams_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/teams"
)

func newServer(t *testing.T, h http.Handler) (*httptest.Server, *teams.Connector) {
	t.Helper()
	srv := httptest.NewServer(h)
	return srv, teams.New(teams.WithHTTPClient(srv.Client()), teams.WithBaseURL(srv.URL))
}

func TestValidate(t *testing.T) {
	t.Parallel()
	c := teams.New()
	if err := c.Validate(context.Background(), connector.ConnectorConfig{}); !errors.Is(err, connector.ErrInvalidConfig) {
		t.Fatalf("err: %v", err)
	}
}

func TestConnectListAndFetch(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case "/me/joinedTeams":
			_, _ = io.WriteString(w, `{"value":[{"id":"team1","displayName":"Eng"}]}`)
		case "/teams/team1/channels":
			_, _ = io.WriteString(w, `{"value":[{"id":"ch1","displayName":"general"}]}`)
		case "/teams/team1/channels/ch1/messages":
			_, _ = io.WriteString(w, `{"value":[{"id":"m1","etag":"e","lastModifiedDateTime":"2024-01-01T00:00:00Z"}]}`)
		case "/teams/team1/channels/ch1/messages/m1":
			_, _ = io.WriteString(w, `{"subject":"hello","createdDateTime":"2024-01-01T00:00:00Z","lastModifiedDateTime":"2024-01-01T00:00:00Z","from":{"user":{"displayName":"Alice"}}}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"tok"}`),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if len(ns) != 1 || ns[0].ID != "team1/ch1" {
		t.Fatalf("ns: %+v", ns)
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
		case r.URL.Path == "/me":
			_, _ = io.WriteString(w, `{"id":"u"}`)
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			_, _ = io.WriteString(w, `{"value":[{"id":"m1","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],"@odata.deltaLink":"NEXT"}`)
		default:
			http.Error(w, r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_token":"x"}`)})
	ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "team1/ch1"}, "")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(ch) != 1 {
		t.Fatalf("changes: %d", len(ch))
	}
	if cur != "NEXT" {
		t.Fatalf("cur: %q", cur)
	}
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	c := teams.New()
	payload := `{"value":[{"changeType":"created","resource":"teams('team1')/channels('ch1')/messages('m1')"},{"changeType":"deleted","resource":"teams('team1')/channels('ch1')/messages('m2')"}]}`
	ch, err := c.HandleWebhook(context.Background(), []byte(payload))
	if err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if len(ch) != 2 {
		t.Fatalf("changes: %d", len(ch))
	}
	if ch[1].Kind != connector.ChangeDeleted {
		t.Fatalf("kinds: %+v", ch)
	}
}
