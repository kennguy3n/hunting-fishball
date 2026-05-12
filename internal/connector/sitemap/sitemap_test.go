package sitemap_test

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
	"github.com/kennguy3n/hunting-fishball/internal/connector/sitemap"
)

func credsFor(t *testing.T, urls ...string) []byte {
	t.Helper()
	b, _ := json.Marshal(sitemap.Credentials{SitemapURLs: urls})

	return b
}

func TestSitemap_Validate(t *testing.T) {
	t.Parallel()
	c := sitemap.New()
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, "https://x/sitemap.xml")}, true},
		{"missing creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"empty urls", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"sitemap_urls":[]}`)}, false},
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

func TestSitemap_ListNamespaces(t *testing.T) {
	t.Parallel()
	c := sitemap.New()
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, "https://a/sitemap.xml", "https://b/sitemap.xml")})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil || len(ns) != 2 {
		t.Fatalf("ns=%+v err=%v", ns, err)
	}
}

func TestSitemap_ListDocuments_BasicAndIndex(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap-index.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>http://`+"REPLACE"+`/child.xml</loc></sitemap>
</sitemapindex>`)
	})
	mux.HandleFunc("/child.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>https://x/a</loc><lastmod>2024-06-01T00:00:00Z</lastmod></url>
<url><loc>https://x/b</loc><lastmod>2024-06-02</lastmod></url>
</urlset>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// Patch the sitemap-index handler to use the live test server origin.
	mux.HandleFunc("/", http.NotFound)

	// Re-register the index handler with the right host:
	childURL := srv.URL + "/child.xml"
	// Rewrite the index body to point to the real child URL.
	// We do this by replacing the placeholder via a wrapper handler.
	rewriter := http.NewServeMux()
	rewriter.HandleFunc("/sitemap-index.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<sitemap><loc>`+childURL+`</loc></sitemap>
</sitemapindex>`)
	})
	rewriter.HandleFunc("/child.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>https://x/a</loc><lastmod>2024-06-01T00:00:00Z</lastmod></url>
<url><loc>https://x/b</loc><lastmod>2024-06-02</lastmod></url>
</urlset>`)
	})
	srv2 := httptest.NewServer(rewriter)
	defer srv2.Close()
	indexURL := srv2.URL + "/sitemap-index.xml"

	c := sitemap.New(sitemap.WithHTTPClient(srv2.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, indexURL)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: indexURL}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	var ids []string
	for it.Next(context.Background()) {
		ids = append(ids, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("err=%v", it.Err())
	}
	if len(ids) != 2 || ids[0] != "https://x/a" || ids[1] != "https://x/b" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestSitemap_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := sitemap.New(sitemap.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL+"/sitemap.xml")})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: srv.URL + "/sitemap.xml"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	for it.Next(context.Background()) {
	}
	if !errors.Is(it.Err(), connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", it.Err())
	}
}

func TestSitemap_FetchDocument(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html><body>hello</body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sitemap.New(sitemap.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL+"/sitemap.xml")})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: srv.URL + "/sitemap.xml", ID: srv.URL + "/page"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	body, _ := io.ReadAll(doc.Content)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("body=%q", body)
	}
}

func TestSitemap_DeltaSync_BootstrapAndIncrement(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
<url><loc>https://x/a</loc><lastmod>2024-06-01T00:00:00Z</lastmod></url>
<url><loc>https://x/b</loc><lastmod>2024-06-03T00:00:00Z</lastmod></url>
</urlset>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := sitemap.New(sitemap.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL+"/sitemap.xml")})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL + "/sitemap.xml"}, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur != "2024-06-03T00:00:00Z" {
		t.Fatalf("cursor=%q", cur)
	}
	changes, _, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL + "/sitemap.xml"}, "2024-06-01T12:00:00Z")
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "https://x/b" {
		t.Fatalf("changes=%+v", changes)
	}
}

func TestSitemap_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := sitemap.New(sitemap.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: credsFor(t, srv.URL+"/sitemap.xml")})
	_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL + "/sitemap.xml"}, "")
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestSitemap_SubscribeNotSupported(t *testing.T) {
	t.Parallel()
	c := sitemap.New()
	_, err := c.Subscribe(context.Background(), nil, connector.Namespace{})
	if !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}
