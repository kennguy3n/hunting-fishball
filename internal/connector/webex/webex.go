// Package webex implements the Webex SourceConnector + DeltaSyncer
// against the Webex Messages API at https://webexapis.com.
// Authentication uses a bearer token (Bot or Integration OAuth).
//
// Credentials must be a JSON blob:
//
//	{
//	 "access_token": "...",  // required
//	 "room_id":      "..."   // required
//	}
//
// The connector exposes the configured Webex room as the single
// namespace. Delta uses `/v1/messages?roomId={id}&before=<id>` —
// Webex orders messages newest-first and supports the `before`
// cursor for pagination; we synthesize a timestamp watermark from
// the most recent message's `created` field.
package webex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// Name is the registry-visible connector name.
const Name = "webex"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	RoomID      string `json:"room_id"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(o *Connector) { o.httpClient = c } }

// WithBaseURL pins the Webex base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://webexapis.com"}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID    string
	sourceID    string
	baseURL     string
	accessToken string
	roomID      string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (o *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.AccessToken == "" {
		return fmt.Errorf("%w: access_token required", connector.ErrInvalidConfig)
	}
	if creds.RoomID == "" {
		return fmt.Errorf("%w: room_id required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /v1/people/me as a cheap auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		baseURL:     o.baseURL,
		accessToken: creds.AccessToken,
		roomID:      creds.RoomID,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/v1/people/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: webex: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webex: people/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the configured room as the only namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("webex: bad connection type")
	}

	return []connector.Namespace{{ID: conn.roomID, Name: conn.roomID, Kind: "room"}}, nil
}

type messageEntry struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created string `json:"created"`
}

type listResponse struct {
	Items []messageEntry `json:"items"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// nextPath holds the next page's path+query, parsed from the
	// Webex `Link: <url>; rel="next"` response header. Empty
	// before the first fetch and once the API stops emitting a
	// next link. Round-22 pagination fix.
	nextPath string
}

func (it *docIterator) Next(ctx context.Context) bool {
	if it.err != nil || it.done {
		return false
	}
	if it.idx >= len(it.page) {
		if !it.fetch(ctx) {
			return false
		}
	}
	if it.idx < len(it.page) {
		it.idx++

		return true
	}

	return false
}

func (it *docIterator) Doc() connector.DocumentRef {
	if it.idx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.idx-1]
}

func (it *docIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *docIterator) Close() error { return nil }

func (it *docIterator) fetch(ctx context.Context) bool {
	// Round-22 pagination fix: follow the Link: <...>; rel="next"
	// header that Webex emits on paginated message responses.
	path := it.nextPath
	if path == "" {
		q := url.Values{}
		q.Set("roomId", it.conn.roomID)
		q.Set("max", "100")
		path = "/v1/messages?" + q.Encode()
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: webex: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("webex: list status=%d", resp.StatusCode)

		return false
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("webex: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, m := range body.Items {
		ref := connector.DocumentRef{NamespaceID: it.conn.roomID, ID: m.ID}
		if ts, perr := time.Parse(time.RFC3339, m.Created); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.nextPath = parseWebexNextLink(resp.Header.Get("Link"))
	it.done = it.nextPath == ""

	return true
}

// parseWebexNextLink extracts the path+query from a Webex
// `Link: <https://...>; rel="next"` header value. Webex returns
// the full URL; we strip the scheme/host so the same `o.do`
// helper can route it through the configured base URL. Round-22
// pagination fix.
func parseWebexNextLink(header string) string {
	if header == "" {
		return ""
	}
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		hasRelNext := false
		for _, p := range segs[1:] {
			p = strings.TrimSpace(p)
			if p == `rel="next"` || p == "rel=next" {
				hasRelNext = true

				break
			}
		}
		if !hasRelNext {
			continue
		}
		raw := strings.TrimSpace(segs[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		if u, err := url.Parse(raw); err == nil && u.Path != "" {
			return u.RequestURI()
		}
	}

	return ""
}

// ListDocuments enumerates Webex messages in the configured room.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("webex: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a single Webex message.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("webex: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/v1/messages/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: webex: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webex: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var m messageEntry
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("webex: decode msg: %w", err)
	}
	updated, _ := time.Parse(time.RFC3339, m.Created)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: conn.roomID, ID: m.ID, UpdatedAt: updated},
		MIMEType:  "text/plain",
		Title:     m.ID,
		Size:      int64(len(m.Text)),
		CreatedAt: updated,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(m.Text)),
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /v1/messages?roomId={id} filtered by RFC 3339
// `created` watermark. Empty cursor returns "now" without
// backfilling.
// Round-23 Devin Review fix: follow Webex's `Link: <url>; rel="next"`
// pagination header across pages. Webex returns messages in
// reverse-chronological order, so we can additionally stop the walk
// once we see a full page where every message is older than the
// cursor — no point fetching further into the past.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("webex: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("webex: bad cursor %q: %w", cursor, err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	q := url.Values{}
	q.Set("roomId", conn.roomID)
	q.Set("max", "100")
	path := "/v1/messages?" + q.Encode()
	for {
		resp, err := o.do(ctx, conn, http.MethodGet, path, nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		linkHdr := resp.Header.Get("Link")
		var body listResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: webex: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("webex: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("webex: decode delta: %w", decodeErr)
		}
		pageHasFresh := false
		for _, m := range body.Items {
			ts, _ := time.Parse(time.RFC3339, m.Created)
			ts = ts.UTC()
			if !ts.After(cursorTime) {
				continue
			}
			pageHasFresh = true
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: m.ID, UpdatedAt: ts}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
			if ts.After(newest) {
				newest = ts
			}
		}
		// Webex returns messages newest-first within a room. Once
		// a full page yields zero fresh records, every subsequent
		// page can only be older — break early.
		if len(body.Items) > 0 && !pageHasFresh {
			break
		}
		nextPath := parseWebexNextLink(linkHdr)
		if nextPath == "" {
			break
		}
		path = nextPath
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("webex: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webex: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
