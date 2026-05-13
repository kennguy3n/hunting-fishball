// Package quip implements the Quip SourceConnector + DeltaSyncer
// against the Quip Automation REST API at https://platform.quip.com.
// Authentication uses a personal-access-token Bearer header.
//
// Credentials must be a JSON blob:
//
//	{
//	 "access_token": "..." // required
//	}
//
// The connector exposes a single synthetic namespace "threads" and
// drives delta off the `updated_usec` field (microseconds since
// epoch) returned by Quip's `/1/threads/recent` endpoint, paged via
// the `max_updated_usec` cursor.
package quip

import (
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
const Name = "quip"

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

// WithBaseURL pins the Quip base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://platform.quip.com",
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID string
	sourceID string
	baseURL  string
	token    string
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

// Connect issues GET /1/users/current as a cheap auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{
		tenantID: cfg.TenantID,
		sourceID: cfg.SourceID,
		baseURL:  o.baseURL,
		token:    creds.AccessToken,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/1/users/current", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: quip: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quip: users/current status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the single synthetic namespace "threads".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "threads", Name: "Threads", Kind: "threads"}}, nil
}

// threadEntry mirrors the subset of Quip's thread JSON the connector
// reads. Quip envelopes a thread under a "thread" key when fetched
// individually but inlines it for `/1/threads/recent`.
type threadEntry struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Author      string `json:"author_id"`
	UpdatedUsec int64  `json:"updated_usec"`
	CreatedUsec int64  `json:"created_usec"`
}

type recentResponse map[string]struct {
	Thread threadEntry `json:"thread"`
	HTML   string      `json:"html"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// maxUpdatedUsec is the Quip cursor; zero asks for the most
	// recent page.
	maxUpdatedUsec int64
}

func (it *docIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	if it.idx >= len(it.page) {
		if it.done {
			return false
		}
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
	const perPage = 50
	q := url.Values{}
	q.Set("count", strconv.Itoa(perPage))
	if it.maxUpdatedUsec > 0 {
		q.Set("max_updated_usec", strconv.FormatInt(it.maxUpdatedUsec, 10))
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/1/threads/recent?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: quip: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("quip: list status=%d", resp.StatusCode)

		return false
	}
	var page recentResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		it.err = fmt.Errorf("quip: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	var oldest int64
	for _, entry := range page {
		t := entry.Thread
		if t.ID == "" {
			continue
		}
		ref := connector.DocumentRef{
			NamespaceID: "threads",
			ID:          t.ID,
			UpdatedAt:   usecToTime(t.UpdatedUsec),
		}
		it.page = append(it.page, ref)
		if oldest == 0 || t.UpdatedUsec < oldest {
			oldest = t.UpdatedUsec
		}
	}
	switch {
	case len(it.page) < perPage:
		it.done = true
	case oldest == 0:
		it.done = true
	default:
		// Advance the cursor backwards so the next page lands at
		// the oldest-thread-minus-one-microsecond. Quip uses an
		// exclusive upper bound on `max_updated_usec`.
		it.maxUpdatedUsec = oldest - 1
	}

	return true
}

// ListDocuments enumerates Quip threads.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, _ connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("quip: bad connection type")
	}

	return &docIterator{o: o, conn: conn}, nil
}

// FetchDocument loads a single Quip thread by ID.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("quip: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/1/threads/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: quip: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quip: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var env struct {
		Thread threadEntry `json:"thread"`
		HTML   string      `json:"html"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("quip: decode fetch: %w", err)
	}
	updated := usecToTime(env.Thread.UpdatedUsec)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "threads", ID: env.Thread.ID, UpdatedAt: updated},
		MIMEType:  "text/html",
		Title:     env.Thread.Title,
		Author:    env.Thread.Author,
		Size:      int64(len(env.HTML)),
		CreatedAt: usecToTime(env.Thread.CreatedUsec),
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(env.HTML)),
	}, nil
}

// Subscribe is unsupported — Quip exposes only polling for content
// changes from the automation API.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /1/threads/recent and surfaces threads with
// `updated_usec` strictly greater than the parsed cursor. The cursor
// is stored as an RFC3339Nano timestamp so that microsecond precision
// (matching Quip's `updated_usec` field) survives a round trip and
// threads updated within the same wall-clock second are not re-emitted
// on subsequent delta cycles. Empty cursor returns "now" without
// backfilling (bootstrap contract).
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("quip: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339Nano), nil
	}
	cursorTime, err := time.Parse(time.RFC3339Nano, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("quip: bad cursor %q: %w", cursor, err)
	}
	cursorUsec := cursorTime.UnixMicro()
	const perPage = 50
	newest := cursorTime
	var changes []connector.DocumentChange
	maxUpdated := int64(0)
	for {
		q := url.Values{}
		q.Set("count", strconv.Itoa(perPage))
		if maxUpdated > 0 {
			q.Set("max_updated_usec", strconv.FormatInt(maxUpdated, 10))
		}
		resp, err := o.do(ctx, conn, http.MethodGet, "/1/threads/recent?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		var page recentResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: quip: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("quip: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("quip: decode delta: %w", decodeErr)
		}
		var oldestUsec int64
		stopped := false
		count := 0
		for _, entry := range page {
			t := entry.Thread
			if t.ID == "" {
				continue
			}
			count++
			if t.UpdatedUsec <= cursorUsec {
				stopped = true

				continue
			}
			ts := usecToTime(t.UpdatedUsec)
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: t.ID, UpdatedAt: ts}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
			if ts.After(newest) {
				newest = ts
			}
			if oldestUsec == 0 || t.UpdatedUsec < oldestUsec {
				oldestUsec = t.UpdatedUsec
			}
		}
		if stopped || count < perPage || oldestUsec == 0 {
			break
		}
		maxUpdated = oldestUsec - 1
	}

	return changes, newest.UTC().Format(time.RFC3339Nano), nil
}

func usecToTime(usec int64) time.Time {
	if usec <= 0 {
		return time.Time{}
	}

	return time.UnixMicro(usec).UTC()
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("quip: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quip: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
