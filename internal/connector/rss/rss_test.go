package rss_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/rss"
)

const atomBody = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example</title>
  <entry>
    <id>e1</id>
    <title>Hello</title>
    <link href="https://example.com/1"/>
    <updated>2024-01-01T00:00:00Z</updated>
    <author><name>alice</name></author>
  </entry>
  <entry>
    <id>e2</id>
    <title>World</title>
    <link href="https://example.com/2"/>
    <updated>2024-02-01T00:00:00Z</updated>
    <author><name>bob</name></author>
  </entry>
</feed>`

const rssBody = `<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0"><channel>
  <title>X</title>
  <item>
    <title>R1</title>
    <link>https://example.com/r1</link>
    <guid>r1</guid>
    <pubDate>Mon, 01 Jan 2024 00:00:00 +0000</pubDate>
    <author>alice@example</author>
  </item>
</channel></rss>`

func credsFor(t *testing.T, url string) []byte {
	t.Helper()
	b, _ := json.Marshal(rss.Credentials{FeedURLs: []string{url}})

	return b
}

func TestRSS_Validate(t *testing.T) {
	t.Parallel()
	r := rss.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, "https://x")}, true},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing feed_urls", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := r.Validate(context.Background(), tc.cfg)
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

func TestRSS_ListDocuments_Atom(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, atomBody)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, err := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, _ := r.ListNamespaces(context.Background(), conn)
	it, _ := r.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "e1" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestRSS_ListDocuments_RSS2(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, rssBody)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, _ := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	ns, _ := r.ListNamespaces(context.Background(), conn)
	it, _ := r.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	count := 0
	for it.Next(context.Background()) {
		count++
	}
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}

func TestRSS_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, _ := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	ns, _ := r.ListNamespaces(context.Background(), conn)
	it, _ := r.ListDocuments(context.Background(), conn, ns[0], connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestRSS_FetchDocument(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, atomBody)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, _ := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	doc, err := r.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: srv.URL, ID: "e1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	if doc.Title != "Hello" || doc.Author != "alice" {
		t.Fatalf("doc=%+v", doc)
	}
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "Hello") {
		t.Fatalf("body=%q", body)
	}
}

func TestRSS_DeltaSync_InitialCursorIsCurrent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, atomBody)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, _ := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	changes, cur, err := r.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("initial should not backfill: %v", changes)
	}
	if cur != "2024-02-01T00:00:00Z" {
		t.Fatalf("cur=%q", cur)
	}
	changes, _, err = r.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL}, "2024-01-15T00:00:00Z")
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "e2" {
		t.Fatalf("changes=%v", changes)
	}
}

func TestRSS_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	r := rss.New(rss.WithHTTPClient(srv.Client()))
	conn, _ := r.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL)})
	_, _, err := r.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited bootstrap, got %v", err)
	}
	_, _, err = r.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL}, "2024-01-01T00:00:00Z")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited steady, got %v", err)
	}
}

func TestRSS_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	r := rss.New()
	_, err := r.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
