// Package sitemap implements a generic SourceConnector +
// DeltaSyncer that fetches and parses XML sitemaps (sitemap.xml
// and sitemap-index files per the sitemaps.org schema). Auth is
// optional — most public sitemaps are anonymous, but the
// connector will send a basic-auth header or a Bearer token if
// credentials are present. We use stdlib net/http and
// encoding/xml only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "sitemap_urls": ["https://example.com/sitemap.xml", ...], // required
//	  "username":     "alice",          // optional
//	  "password":     "...",            // optional
//	  "bearer_token": "..."             // optional, takes priority over basic auth
//	}
//
// Each configured sitemap URL becomes a synthetic namespace.
// Incremental synchronisation reads each entry's `<lastmod>`
// timestamp; a multi-format parser accepts RFC3339, RFC3339Nano,
// "2006-01-02", "2006-01-02T15:04:05" and a few common
// W3C-datetime variants.
package sitemap

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// Name is the registry-visible connector name.
const Name = "sitemap"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	SitemapURLs []string `json:"sitemap_urls"`
	Username    string   `json:"username,omitempty"`
	Password    string   `json:"password,omitempty"`
	BearerToken string   `json:"bearer_token,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(s *Connector) { s.httpClient = c } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	s := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

type connection struct {
	tenantID    string
	sourceID    string
	sitemaps    []string
	username    string
	password    string
	bearerToken string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (s *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
	if cfg.TenantID == "" {
		return fmt.Errorf("%w: tenant_id required", connector.ErrInvalidConfig)
	}
	if cfg.SourceID == "" {
		return fmt.Errorf("%w: source_id required", connector.ErrInvalidConfig)
	}
	if len(cfg.Credentials) == 0 {
		return fmt.Errorf("%w: credentials required", connector.ErrInvalidConfig)
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if len(creds.SitemapURLs) == 0 {
		return fmt.Errorf("%w: sitemap_urls required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect stores the sitemap list; there is no upstream "user" call.
func (s *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := s.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}

	return &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		sitemaps:    creds.SitemapURLs,
		username:    creds.Username,
		password:    creds.Password,
		bearerToken: creds.BearerToken,
	}, nil
}

// ListNamespaces returns one namespace per configured sitemap.
func (s *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sitemap: bad connection type")
	}
	out := make([]connector.Namespace, 0, len(conn.sitemaps))
	for _, u := range conn.sitemaps {
		out = append(out, connector.Namespace{ID: u, Name: u, Kind: "sitemap"})
	}

	return out, nil
}

type entry struct {
	Loc     string
	LastMod time.Time
}

type sitemapIterator struct {
	page    []connector.DocumentRef
	pageIdx int
	loaded  bool
	err     error
	s       *Connector
	conn    *connection
	ns      connector.Namespace
}

func (it *sitemapIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	if !it.loaded {
		if !it.fetch(ctx) {
			return false
		}
		it.loaded = true
	}
	if it.pageIdx < len(it.page) {
		it.pageIdx++

		return true
	}

	return false
}

func (it *sitemapIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *sitemapIterator) Err() error {
	if it.err == nil && it.loaded {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *sitemapIterator) Close() error { return nil }

func (it *sitemapIterator) fetch(ctx context.Context) bool {
	entries, err := it.s.fetchEntries(ctx, it.conn, it.ns.ID)
	if err != nil {
		it.err = err

		return false
	}
	for _, e := range entries {
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          e.Loc,
			UpdatedAt:   e.LastMod,
		})
	}

	return true
}

// ListDocuments enumerates entries from the sitemap namespace.
func (s *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sitemap: bad connection type")
	}

	return &sitemapIterator{s: s, conn: conn, ns: ns}, nil
}

// FetchDocument fetches the URL named in DocumentRef.ID and
// returns its body as the document content.
func (s *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sitemap: bad connection type")
	}
	resp, err := s.do(ctx, conn, ref.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: sitemap: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sitemap: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sitemap: read body: %w", err)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "text/html"
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  mime,
		Title:     ref.ID,
		Size:      int64(len(raw)),
		CreatedAt: ref.UpdatedAt,
		UpdatedAt: ref.UpdatedAt,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"sitemap_url": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — sitemaps are poll-only.
func (s *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (s *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync re-fetches the sitemap and returns entries with a
// `<lastmod>` strictly greater than the cursor (RFC3339). Empty
// cursor on the first call returns the newest entry's timestamp
// (or "now") without backfilling.
func (s *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("sitemap: bad connection type")
	}
	entries, err := s.fetchEntries(ctx, conn, ns.ID)
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		newest := time.Now().UTC()
		var maxT time.Time
		for _, e := range entries {
			if e.LastMod.After(maxT) {
				maxT = e.LastMod
			}
		}
		if !maxT.IsZero() {
			newest = maxT
		}

		return nil, newest.UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("sitemap: bad cursor %q: %w", cursor, err)
	}
	var changes []connector.DocumentChange
	newest := cursorTime
	for _, e := range entries {
		if !e.LastMod.After(cursorTime) {
			continue
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          e.Loc,
				UpdatedAt:   e.LastMod,
			},
		})
		if e.LastMod.After(newest) {
			newest = e.LastMod
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// fetchEntries downloads the sitemap and recursively expands
// any nested <sitemap> entries (a sitemap-index file).
func (s *Connector) fetchEntries(ctx context.Context, conn *connection, url string) ([]entry, error) {
	resp, err := s.do(ctx, conn, url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: sitemap: get %s status=%d", connector.ErrRateLimited, url, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sitemap: get %s status=%d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sitemap: read %s: %w", url, err)
	}
	type rawURL struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod"`
	}
	var idx struct {
		XMLName xml.Name `xml:"sitemapindex"`
		Sitemap []rawURL `xml:"sitemap"`
	}
	var us struct {
		XMLName xml.Name `xml:"urlset"`
		URL     []rawURL `xml:"url"`
	}
	if err := xml.Unmarshal(raw, &idx); err == nil && len(idx.Sitemap) > 0 {
		var all []entry
		for _, sm := range idx.Sitemap {
			child, err := s.fetchEntries(ctx, conn, sm.Loc)
			if err != nil {
				return nil, err
			}
			all = append(all, child...)
		}

		return all, nil
	}
	if err := xml.Unmarshal(raw, &us); err != nil {
		return nil, fmt.Errorf("sitemap: parse %s: %w", url, err)
	}
	out := make([]entry, 0, len(us.URL))
	for _, u := range us.URL {
		out = append(out, entry{
			Loc:     u.Loc,
			LastMod: parseLastMod(u.LastMod),
		})
	}

	return out, nil
}

// do fetches the URL with the configured auth, if any.
func (s *Connector) do(ctx context.Context, conn *connection, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("sitemap: build request: %w", err)
	}
	if conn.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+conn.bearerToken)
	} else if conn.username != "" || conn.password != "" {
		req.SetBasicAuth(conn.username, conn.password)
	}
	req.Header.Set("Accept", "application/xml, text/xml;q=0.9, */*;q=0.5")
	req.Header.Set("User-Agent", "hunting-fishball-sitemap/1")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sitemap: get %s: %w", url, err)
	}

	return resp, nil
}

// parseLastMod accepts the common <lastmod> formats: RFC3339,
// "2006-01-02", "2006-01-02T15:04:05", RFC3339Nano. Returns zero
// time on unparseable input — the entry still surfaces via
// ListDocuments / FetchDocument; only DeltaSync will skip it.
func parseLastMod(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC()
		}
	}

	return time.Time{}
}

// Register registers the Sitemap connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
