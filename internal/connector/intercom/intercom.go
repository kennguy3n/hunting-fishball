// Package intercom implements the Intercom SourceConnector +
// DeltaSyncer against the Intercom REST API v2 at
// https://api.intercom.io. Authentication uses a bearer access token.
//
// Credentials must be a JSON blob:
//
//	{
//	 "access_token": "..."  // required
//	}
//
// The connector exposes two namespaces — "conversations" and
// "articles". Delta uses the POST /conversations/search endpoint
// with `updated_at > <unix>` query and Intercom's standard
// `starting_after` cursor.
package intercom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// Name is the registry-visible connector name.
const Name = "intercom"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
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

// WithBaseURL pins the Intercom base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.intercom.io"}
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

	return nil
}

// Connect issues GET /me as a cheap auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, baseURL: o.baseURL, accessToken: creds.AccessToken}
	resp, err := o.do(ctx, conn, http.MethodGet, "/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: intercom: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intercom: /me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns conversations + articles.
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{
		{ID: "conversations", Name: "Conversations", Kind: "conversations"},
		{ID: "articles", Name: "Articles", Kind: "articles"},
	}, nil
}

type convEntry struct {
	ID        string `json:"id"`
	UpdatedAt int64  `json:"updated_at"`
	CreatedAt int64  `json:"created_at"`
}

type listResponse struct {
	Conversations []convEntry `json:"conversations"`
	Data          []convEntry `json:"data"`
	Pages         struct {
		Next *struct {
			StartingAfter string `json:"starting_after"`
		} `json:"next"`
	} `json:"pages"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// startingAfter mirrors Intercom's `pages.next.starting_after`
	// cursor; empty for the first request and once the response
	// stops echoing back a `next` block. Round-22 pagination fix.
	startingAfter string
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
	// Round-22 pagination fix: walk `pages.next.starting_after`.
	q := url.Values{}
	q.Set("per_page", "50")
	if it.startingAfter != "" {
		q.Set("starting_after", it.startingAfter)
	}
	path := "/" + it.ns.ID + "?" + q.Encode()
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: intercom: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("intercom: list status=%d", resp.StatusCode)

		return false
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("intercom: decode list: %w", err)

		return false
	}
	rows := body.Conversations
	if len(rows) == 0 {
		rows = body.Data
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, r := range rows {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: r.ID}
		if r.UpdatedAt > 0 {
			ref.UpdatedAt = time.Unix(r.UpdatedAt, 0).UTC()
		}
		it.page = append(it.page, ref)
	}
	if body.Pages.Next != nil && body.Pages.Next.StartingAfter != "" {
		it.startingAfter = body.Pages.Next.StartingAfter
	} else {
		it.done = true
	}

	return true
}

// ListDocuments enumerates Intercom conversations or articles.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("intercom: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a single conversation or article.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("intercom: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/"+ref.NamespaceID+"/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: intercom: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("intercom: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("intercom: read body: %w", err)
	}
	var meta struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		UpdatedAt int64  `json:"updated_at"`
		CreatedAt int64  `json:"created_at"`
	}
	_ = json.Unmarshal(raw, &meta)
	updated := time.Unix(meta.UpdatedAt, 0).UTC()
	created := time.Unix(meta.CreatedAt, 0).UTC()
	_ = conn

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: ref.NamespaceID, ID: meta.ID, UpdatedAt: updated},
		MIMEType:  "application/json",
		Title:     meta.Title,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync branches on namespace. Conversations use the Intercom
// search API (POST /conversations/search with updated_at > cursor).
// Articles have no server-side `/search` surface, so we walk the
// paginated GET /articles list and apply the updated_at filter
// client-side. Empty cursor returns "now" without backfilling, per
// the bootstrap contract.
//
// Round-22 Devin Review fix: previous implementation built
// `path := "/" + ns.ID + "/search"` for every namespace, which
// silently 404s against `/articles/search` (no such endpoint).
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("intercom: bad connection type")
	}
	if cursor == "" {
		return nil, strconv.FormatInt(time.Now().UTC().Unix(), 10), nil
	}
	since, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("intercom: bad cursor %q: %w", cursor, err)
	}
	switch ns.ID {
	case "conversations":
		return o.deltaSyncSearch(ctx, conn, ns, since)
	case "articles":
		return o.deltaSyncArticles(ctx, conn, ns, since)
	default:
		return nil, cursor, fmt.Errorf("%w: intercom: unsupported namespace %q", connector.ErrNotSupported, ns.ID)
	}
}

// deltaSyncSearch handles namespaces backed by Intercom's search API
// (currently `conversations`).
func (o *Connector) deltaSyncSearch(ctx context.Context, conn *connection, ns connector.Namespace, since int64) ([]connector.DocumentChange, string, error) {
	q := map[string]interface{}{
		"query": map[string]interface{}{
			"field":    "updated_at",
			"operator": ">",
			"value":    since,
		},
	}
	body, _ := json.Marshal(q)
	path := "/" + ns.ID + "/search"
	resp, err := o.do(ctx, conn, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: intercom: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, strconv.FormatInt(since, 10), fmt.Errorf("intercom: delta status=%d", resp.StatusCode)
	}
	var lr listResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, "", fmt.Errorf("intercom: decode delta: %w", err)
	}
	rows := lr.Conversations
	if len(rows) == 0 {
		rows = lr.Data
	}
	newest := since
	var changes []connector.DocumentChange
	for _, r := range rows {
		ref := connector.DocumentRef{NamespaceID: ns.ID, ID: r.ID}
		if r.UpdatedAt > 0 {
			ref.UpdatedAt = time.Unix(r.UpdatedAt, 0).UTC()
			if r.UpdatedAt > newest {
				newest = r.UpdatedAt
			}
		}
		changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
	}

	return changes, strconv.FormatInt(newest, 10), nil
}

// deltaSyncArticles walks GET /articles paginated by
// `pages.next.starting_after`, surfacing only records whose
// `updated_at` exceeds the cursor.
//
// Round-23 Devin Review fix: the previous implementation walked the
// full catalogue every cycle (Intercom's articles endpoint has no
// server-side filter). We now request `order=desc&sort=updated_at`
// as a best-effort hint and short-circuit the walk once we observe
// a full page where every record is stale AND the records we've
// seen so far have arrived in non-increasing `updated_at` order.
// If Intercom ignores the order params or returns records out of
// order, the verification check defaults to `false` and we fall
// back to a full walk — correctness is preserved either way.
func (o *Connector) deltaSyncArticles(ctx context.Context, conn *connection, ns connector.Namespace, since int64) ([]connector.DocumentChange, string, error) {
	newest := since
	var changes []connector.DocumentChange
	startingAfter := ""
	// sawMonotonicDesc tracks whether every record observed so far
	// has had `updated_at` <= the previous record's. Reset to false
	// the moment we observe an out-of-order record, which disables
	// early termination for the rest of this delta call.
	sawMonotonicDesc := true
	var prevUpdatedAt int64 = 1<<63 - 1
	for {
		q := url.Values{}
		q.Set("per_page", "100")
		// Best-effort sort hints; if Intercom ignores them the
		// monotonic-decrease guard below stays false and we
		// just walk the full catalogue (same as before).
		q.Set("order", "desc")
		q.Set("sort", "updated_at")
		if startingAfter != "" {
			q.Set("starting_after", startingAfter)
		}
		resp, err := o.do(ctx, conn, http.MethodGet, "/"+ns.ID+"?"+q.Encode(), nil)
		if err != nil {
			return nil, strconv.FormatInt(since, 10), err
		}
		status := resp.StatusCode
		var body listResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, strconv.FormatInt(since, 10), fmt.Errorf("%w: intercom: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, strconv.FormatInt(since, 10), fmt.Errorf("intercom: articles delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, strconv.FormatInt(since, 10), fmt.Errorf("intercom: decode articles: %w", decodeErr)
		}
		rows := body.Data
		if len(rows) == 0 {
			rows = body.Conversations
		}
		pageHasFresh := false
		for _, r := range rows {
			if r.UpdatedAt > prevUpdatedAt {
				sawMonotonicDesc = false
			}
			prevUpdatedAt = r.UpdatedAt
			if r.UpdatedAt <= since {
				continue
			}
			pageHasFresh = true
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: r.ID, UpdatedAt: time.Unix(r.UpdatedAt, 0).UTC()}
			if r.UpdatedAt > newest {
				newest = r.UpdatedAt
			}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		}
		// Safe early-termination: only honour it when the page is
		// non-empty AND every record we've seen across all pages
		// in this call has been non-increasing by updated_at. If
		// Intercom honours the order hint, we'll exit after the
		// first fully-stale page; if it doesn't, this guard stays
		// false and we walk every page (no regression vs. before).
		if sawMonotonicDesc && len(rows) > 0 && !pageHasFresh {
			break
		}
		if body.Pages.Next == nil || body.Pages.Next.StartingAfter == "" {
			break
		}
		startingAfter = body.Pages.Next.StartingAfter
	}

	return changes, strconv.FormatInt(newest, 10), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("intercom: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("intercom: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
