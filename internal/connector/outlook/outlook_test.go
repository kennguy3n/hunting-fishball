package outlook_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/outlook"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(outlook.Credentials{AccessToken: "tok"})

	return b
}

func TestOutlook_Validate(t *testing.T) {
	t.Parallel()
	c := outlook.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
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

func TestOutlook_Connect_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestOutlook_ListNamespaces(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"id":"AAA","displayName":"Inbox"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 1 || ns[0].ID != "AAA" {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestOutlook_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()
	var nextURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skiptoken") == "n1" {
			_, _ = io.WriteString(w, `{"value":[{"id":"M2","lastModifiedDateTime":"2024-01-02T00:00:00Z"}]}`)

			return
		}
		_, _ = io.WriteString(w, `{"value":[{"id":"M1","lastModifiedDateTime":"2024-01-01T00:00:00Z"}],"@odata.nextLink":"`+nextURL+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	nextURL = srv.URL + "/me/mailFolders/INBOX/messages?$skiptoken=n1"
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "INBOX"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "M1" || ids[1] != "M2" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestOutlook_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "INBOX"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestOutlook_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/messages/M1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"M1","subject":"hi","bodyPreview":"hi there","lastModifiedDateTime":"2024-01-02T00:00:00Z","receivedDateTime":"2024-01-01T00:00:00Z","from":{"emailAddress":{"address":"alice@x","name":"Alice"}}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "INBOX", ID: "M1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "hi" || doc.Author != "alice@x" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hi there") {
		t.Fatalf("body=%q", body)
	}
}

func TestOutlook_DeltaSync_BootstrapReturnsDeltaLink(t *testing.T) {
	t.Parallel()
	var deltaLink string
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[],"@odata.deltaLink":"`+deltaLink+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	deltaLink = srv.URL + "/me/mailFolders/INBOX/messages/delta?$deltatoken=NEW"
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "")
	if err != nil {
		t.Fatalf("DeltaSync bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur != deltaLink {
		t.Fatalf("cursor=%q want %q", cur, deltaLink)
	}
}

func TestOutlook_DeltaSync_RemovedMappedToDeleted(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[
			{"id":"M1","lastModifiedDateTime":"2024-01-01T00:00:00Z"},
			{"id":"M2","@removed":{}}
		],"@odata.deltaLink":"http://example/delta?$deltatoken=Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, srv.URL+"/me/mailFolders/INBOX/messages/delta?$deltatoken=PREV")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 || changes[0].Kind != connector.ChangeUpserted || changes[1].Kind != connector.ChangeDeleted {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestOutlook_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages/delta", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestOutlook_FetchDocument_EscapesMessageID(t *testing.T) {
	t.Parallel()
	const rawID = "AAMk+Adi/Bl=="
	var seenPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{"id":"x","subject":"hi"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "INBOX", ID: rawID})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	// Graph message IDs are base64 and routinely contain '/', which
	// must be %2F-escaped to avoid path-segment confusion. '+' and '='
	// are sub-delims and RFC-3986-safe in path segments, so PathEscape
	// leaves them as-is — we only assert the dangerous '/' is escaped.
	if strings.Contains(seenPath, "Adi/Bl") || !strings.Contains(seenPath, "%2F") {
		t.Fatalf("server saw unescaped message ID; path=%q", seenPath)
	}
}

func TestOutlook_DeltaSync_PaginationAggregates(t *testing.T) {
	t.Parallel()
	var (
		nextLink  string
		deltaLink string
		hits      [2]int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/me/mailFolders/INBOX/messages/delta", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("$skiptoken") == "P2" {
			hits[1]++
			_, _ = io.WriteString(w, `{"value":[
				{"id":"M3","lastModifiedDateTime":"2024-01-03T00:00:00Z"},
				{"id":"M4","@removed":{}}
			],"@odata.deltaLink":"`+deltaLink+`"}`)

			return
		}
		hits[0]++
		_, _ = io.WriteString(w, `{"value":[
			{"id":"M1","lastModifiedDateTime":"2024-01-01T00:00:00Z"},
			{"id":"M2","lastModifiedDateTime":"2024-01-02T00:00:00Z"}
		],"@odata.nextLink":"`+nextLink+`"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	nextLink = srv.URL + "/me/mailFolders/INBOX/messages/delta?$skiptoken=P2"
	deltaLink = srv.URL + "/me/mailFolders/INBOX/messages/delta?$deltatoken=NEW"
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, srv.URL+"/me/mailFolders/INBOX/messages/delta?$deltatoken=PREV")
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
		connector.ChangeUpserted, // M1
		connector.ChangeUpserted, // M2
		connector.ChangeUpserted, // M3 page 2
		connector.ChangeDeleted,  // M4 @removed page 2
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

func TestOutlook_UserPath_EscapesUserID(t *testing.T) {
	t.Parallel()
	const rawUser = "weird/user@acme.com"
	var seenPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.EscapedPath()
		_, _ = io.WriteString(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := outlook.New(outlook.WithBaseURL(srv.URL), outlook.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(outlook.Credentials{AccessToken: "tok", UserID: rawUser})
	if _, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// user_id is base64/email-shaped in production but defense-in-depth:
	// any '/' in c.user must be %2F-escaped so it can't break the path.
	if strings.Contains(seenPath, "weird/user") || !strings.Contains(seenPath, "%2F") {
		t.Fatalf("server saw unescaped user_id; path=%q", seenPath)
	}
}

func TestOutlook_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := outlook.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
