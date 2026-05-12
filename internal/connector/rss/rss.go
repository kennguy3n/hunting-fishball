// Package rss implements a generic SourceConnector + DeltaSyncer
// that polls RSS 2.0 and Atom 1.0 feeds. Authentication is
// optional — most public feeds are anonymous, but the connector
// will send a basic-auth header if credentials are present. We
// use stdlib net/http and encoding/xml only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "feed_urls": ["https://blog.example.com/feed", ...], // required
//	  "username":  "alice",                                // optional
//	  "password":  "..."                                   // optional
//	}
package rss

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
const Name = "rss"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	FeedURLs []string `json:"feed_urls"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(r *Connector) { r.httpClient = c } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	r := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(r)
	}

	return r
}

type connection struct {
	tenantID string
	sourceID string
	feeds    []string
	username string
	password string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (r *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if len(creds.FeedURLs) == 0 {
		return fmt.Errorf("%w: feed_urls required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect stores the feed list; there is no upstream "user" call.
func (r *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := r.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}

	return &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, feeds: creds.FeedURLs, username: creds.Username, password: creds.Password}, nil
}

// ListNamespaces returns one namespace per configured feed URL.
func (r *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("rss: bad connection type")
	}
	out := make([]connector.Namespace, 0, len(conn.feeds))
	for _, f := range conn.feeds {
		out = append(out, connector.Namespace{ID: f, Name: f, Kind: "feed"})
	}

	return out, nil
}

// feedIterator yields entries from a single fetched feed.
type feedIterator struct {
	page    []connector.DocumentRef
	pageIdx int
	err     error
	loaded  bool
	r       *Connector
	conn    *connection
	ns      connector.Namespace
}

func (it *feedIterator) Next(ctx context.Context) bool {
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

func (it *feedIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *feedIterator) Err() error {
	if it.err == nil && it.loaded {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *feedIterator) Close() error { return nil }

func (it *feedIterator) fetch(ctx context.Context) bool {
	entries, err := it.r.fetchEntries(ctx, it.conn, it.ns.ID)
	if err != nil {
		it.err = err

		return false
	}
	for _, e := range entries {
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          e.ID,
			ETag:        e.Updated.UTC().Format(time.RFC3339),
			UpdatedAt:   e.Updated,
		})
	}

	return true
}

// ListDocuments enumerates entries from the feed namespace.
func (r *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("rss: bad connection type")
	}

	return &feedIterator{r: r, conn: conn, ns: ns}, nil
}

// FetchDocument re-fetches the feed and returns the requested entry.
func (r *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("rss: bad connection type")
	}
	entries, err := r.fetchEntries(ctx, conn, ref.NamespaceID)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID != ref.ID {
			continue
		}
		raw, _ := json.Marshal(e)

		return &connector.Document{
			Ref:       ref,
			MIMEType:  "application/json",
			Title:     e.Title,
			Author:    e.Author,
			Size:      int64(len(raw)),
			CreatedAt: e.Updated,
			UpdatedAt: e.Updated,
			Content:   io.NopCloser(strings.NewReader(string(raw))),
			Metadata:  map[string]string{"feed_url": ref.NamespaceID, "link": e.Link},
		}, nil
	}

	return nil, fmt.Errorf("rss: entry not found: %s", ref.ID)
}

// Subscribe is unsupported — RSS is poll-only by definition.
func (r *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (r *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync re-fetches the feed and returns entries with an
// Updated/Published value strictly greater than the cursor
// (RFC3339). Empty cursor on the first call returns the newest
// entry's timestamp without backfilling history.
func (r *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("rss: bad connection type")
	}
	entries, err := r.fetchEntries(ctx, conn, ns.ID)
	if err != nil {
		return nil, "", err
	}
	if cursor == "" {
		newest := time.Now().UTC().Format(time.RFC3339)
		for _, e := range entries {
			ts := e.Updated.UTC().Format(time.RFC3339)
			if ts > newest {
				newest = ts
			}
		}
		// If the feed had entries, use the latest seen instead of now.
		if len(entries) > 0 {
			var maxTime time.Time
			for _, e := range entries {
				if e.Updated.After(maxTime) {
					maxTime = e.Updated
				}
			}
			if !maxTime.IsZero() {
				newest = maxTime.UTC().Format(time.RFC3339)
			}
		}

		return nil, newest, nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("rss: bad cursor %q: %w", cursor, err)
	}
	var changes []connector.DocumentChange
	newestSeen := cursorTime
	for _, e := range entries {
		if !e.Updated.After(cursorTime) {
			continue
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          e.ID,
				ETag:        e.Updated.UTC().Format(time.RFC3339),
				UpdatedAt:   e.Updated,
			},
		})
		if e.Updated.After(newestSeen) {
			newestSeen = e.Updated
		}
	}

	return changes, newestSeen.UTC().Format(time.RFC3339), nil
}

// fetchEntries downloads the feed and normalises RSS 2.0 and Atom 1.0.
type feedEntry struct {
	ID      string
	Title   string
	Link    string
	Author  string
	Updated time.Time
}

func (r *Connector) fetchEntries(ctx context.Context, conn *connection, url string) ([]feedEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("rss: build request: %w", err)
	}
	if conn.username != "" || conn.password != "" {
		req.SetBasicAuth(conn.username, conn.password)
	}
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.8, text/xml;q=0.7")
	req.Header.Set("User-Agent", "hunting-fishball-rss/1")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rss: get %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: rss: status=%d url=%s", connector.ErrRateLimited, resp.StatusCode, url)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rss: %s status=%d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("rss: read body: %w", err)
	}

	return parseFeed(raw)
}

func parseFeed(raw []byte) ([]feedEntry, error) {
	// Try Atom first.
	var atom atomFeed
	if err := xml.Unmarshal(raw, &atom); err == nil && atom.XMLName.Local == "feed" {
		out := make([]feedEntry, 0, len(atom.Entries))
		for _, e := range atom.Entries {
			updated := parseTimeAny(e.Updated, e.Published)
			id := e.ID
			if id == "" {
				id = e.Link.Href
			}
			out = append(out, feedEntry{
				ID: id, Title: e.Title, Link: e.Link.Href,
				Author: e.Author.Name, Updated: updated,
			})
		}

		return out, nil
	}
	var rss rssFeed
	if err := xml.Unmarshal(raw, &rss); err == nil && rss.XMLName.Local == "rss" {
		out := make([]feedEntry, 0, len(rss.Channel.Items))
		for _, i := range rss.Channel.Items {
			updated := parseTimeAny(i.PubDate, i.Updated)
			id := i.GUID
			if id == "" {
				id = i.Link
			}
			out = append(out, feedEntry{
				ID: id, Title: i.Title, Link: i.Link,
				Author: i.Author, Updated: updated,
			})
		}

		return out, nil
	}

	return nil, errors.New("rss: feed is neither RSS 2.0 nor Atom 1.0")
}

func parseTimeAny(candidates ...string) time.Time {
	formats := []string{time.RFC3339, time.RFC1123Z, time.RFC1123, "2006-01-02T15:04:05Z07:00"}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		for _, f := range formats {
			if t, err := time.Parse(f, c); err == nil {
				return t.UTC()
			}
		}
	}

	return time.Time{}
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string   `xml:"id"`
	Title     string   `xml:"title"`
	Link      atomLink `xml:"link"`
	Updated   string   `xml:"updated"`
	Published string   `xml:"published"`
	Author    struct {
		Name string `xml:"name"`
	} `xml:"author"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
}

type rssFeed struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title   string `xml:"title"`
	Link    string `xml:"link"`
	GUID    string `xml:"guid"`
	PubDate string `xml:"pubDate"`
	Updated string `xml:"updated"`
	Author  string `xml:"author"`
}

// Register registers the RSS connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
