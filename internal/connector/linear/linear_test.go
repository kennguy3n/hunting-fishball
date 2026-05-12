package linear_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(linear.Credentials{APIKey: "lin_api_test"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

// gqlServer returns an httptest.Server that decodes the inbound
// GraphQL POST body, looks up the operation by a sub-string match
// on the query string, and returns the matching response.
func gqlServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		for needle, resp := range responses {
			if strings.Contains(req.Query, needle) {
				_, _ = w.Write([]byte(resp))

				return
			}
		}
		http.Error(w, "no match", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestLinear_Validate(t *testing.T) {
	t.Parallel()

	l := linear.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := l.Validate(context.Background(), tc.cfg)
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

func TestLinear_Connect(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer": `{"data":{"viewer":{"id":"U1"}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestLinear_Connect_AuthFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	if _, err := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestLinear_ListNamespaces(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer": `{"data":{"viewer":{"id":"U1"}}}`,
		"teams":  `{"data":{"teams":{"nodes":[{"id":"T1","key":"ENG","name":"Engineering"}]}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := l.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 1 || ns[0].Kind != "team" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestLinear_ListDocuments(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":       `{"data":{"viewer":{"id":"U1"}}}`,
		"issues(first": `{"data":{"team":{"issues":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"I1","identifier":"ENG-1","updatedAt":"2024-01-01T00:00:00Z"},{"id":"I2","identifier":"ENG-2","updatedAt":"2024-01-02T00:00:00Z"}]}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, err := l.ListDocuments(context.Background(), conn, connector.Namespace{ID: "T1"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
	if len(ids) != 2 {
		t.Fatalf("ids: %v", ids)
	}
}

func TestLinear_FetchDocument(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":    `{"data":{"viewer":{"id":"U1"}}}`,
		"issue(id:": `{"data":{"issue":{"id":"I1","identifier":"ENG-1","title":"Test","description":"hello","createdAt":"2024-01-01T00:00:00Z","updatedAt":"2024-01-02T00:00:00Z","creator":{"name":"alice","email":"a@b.com"}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := l.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "T1", ID: "I1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if string(body) != "hello" {
		t.Fatalf("body: %q", body)
	}
	if doc.Metadata["identifier"] != "ENG-1" {
		t.Fatalf("meta: %+v", doc.Metadata)
	}
}

func TestLinear_DeltaSync(t *testing.T) {
	t.Parallel()

	srv := gqlServer(t, map[string]string{
		"viewer":             `{"data":{"viewer":{"id":"U1"}}}`,
		"orderBy: updatedAt": `{"data":{"team":{"issues":{"nodes":[{"id":"I1","identifier":"ENG-1","updatedAt":"2024-01-02T00:00:00Z"},{"id":"I2","identifier":"ENG-2","updatedAt":"2024-01-03T00:00:00Z"}]}}}}`,
	})
	l := linear.New(linear.WithBaseURL(srv.URL), linear.WithHTTPClient(srv.Client()))
	conn, _ := l.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if len(changes) != 0 || cur != "2024-01-03T00:00:00Z" {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = l.DeltaSync(context.Background(), conn, connector.Namespace{ID: "T1"}, "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 2 || cur != "2024-01-03T00:00:00Z" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

func TestLinear_HandleWebhook(t *testing.T) {
	t.Parallel()

	l := linear.New()
	for _, tc := range []struct {
		name    string
		payload string
		want    int
		kind    connector.ChangeKind
	}{
		{"create", `{"action":"create","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-01T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeUpserted},
		{"update", `{"action":"update","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-02T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeUpserted},
		{"remove", `{"action":"remove","type":"Issue","data":{"id":"I1","updatedAt":"2024-01-03T00:00:00Z","team":{"id":"T1"}}}`, 1, connector.ChangeDeleted},
		{"other-type", `{"action":"create","type":"Comment","data":{"id":"C1"}}`, 0, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changes, err := l.HandleWebhook(context.Background(), []byte(tc.payload))
			if err != nil {
				t.Fatalf("HandleWebhook: %v", err)
			}
			if len(changes) != tc.want {
				t.Fatalf("count: got %d want %d", len(changes), tc.want)
			}
			if tc.want > 0 && changes[0].Kind != tc.kind {
				t.Fatalf("kind: got %d want %d", changes[0].Kind, tc.kind)
			}
		})
	}
}

func TestLinear_HandleWebhook_Empty(t *testing.T) {
	t.Parallel()

	l := linear.New()
	if _, err := l.HandleWebhook(context.Background(), nil); err == nil {
		t.Fatal("expected error on empty payload")
	}
}

func TestLinear_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	if _, err := linear.New().Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestLinear_WebhookPath(t *testing.T) {
	t.Parallel()

	if got := linear.New().WebhookPath(); got != "/linear" {
		t.Fatalf("WebhookPath: %q", got)
	}
}

func TestLinear_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector(linear.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
