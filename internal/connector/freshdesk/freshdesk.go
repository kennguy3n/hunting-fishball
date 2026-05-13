// Package freshdesk implements the Freshdesk SourceConnector +
// DeltaSyncer against the Freshdesk REST API v2 at
// https://<domain>.freshdesk.com/api/v2. Authentication uses HTTP
// basic auth with the API key as the username and the literal "X"
// as the password (the same pattern BambooHR uses).
//
// Credentials must be a JSON blob:
//
//	{
//	 "domain":  "acme",   // required
//	 "api_key": "..."     // required
//	}
//
// The connector exposes a single synthetic namespace "tickets".
// Delta is driven by `/api/v2/tickets?updated_since=<ISO8601>` with
// `page` offset pagination (Freshdesk caps at 30 pages × 100 items).
package freshdesk

import (
	"context"
	"encoding/base64"
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
const Name = "freshdesk"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Domain string `json:"domain"`
	APIKey string `json:"api_key"`
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

// WithBaseURL pins the Freshdesk base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID string
	sourceID string
	baseURL  string
	authHdr  string
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
	if creds.Domain == "" && o.baseURL == "" {
		return fmt.Errorf("%w: domain required", connector.ErrInvalidConfig)
	}
	if creds.APIKey == "" {
		return fmt.Errorf("%w: api_key required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /api/v2/agents/me as a cheap auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := o.baseURL
	if base == "" {
		base = "https://" + creds.Domain + ".freshdesk.com"
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(creds.APIKey+":X"))
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, baseURL: base, authHdr: auth}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v2/agents/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: freshdesk: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("freshdesk: agents/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the single synthetic namespace "tickets".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "tickets", Name: "Tickets", Kind: "tickets"}}, nil
}

type ticketEntry struct {
	ID          int64  `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description_text"`
	UpdatedAt   string `json:"updated_at"`
	CreatedAt   string `json:"created_at"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// nextPage is the 1-indexed page number sent to Freshdesk's
	// `?page=N` parameter. Round-22 pagination fix.
	nextPage int
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
	// Round-22 pagination fix: walk Freshdesk's 1-indexed `page`
	// parameter until we receive fewer than `per_page` records.
	// Round-23 Devin Review fix: also short-circuit on the absence
	// of a `Link: rel="next"` response header so the case where
	// total count is exactly N×perPage doesn't fire a wasted
	// trailing empty request.
	const perPage = 100
	if it.nextPage == 0 {
		it.nextPage = 1
	}
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("page", strconv.Itoa(it.nextPage))
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/api/v2/tickets?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	status := resp.StatusCode
	linkHdr := resp.Header.Get("Link")
	var tickets []ticketEntry
	decodeErr := json.NewDecoder(resp.Body).Decode(&tickets)
	_ = resp.Body.Close()
	if status == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: freshdesk: list status=%d", connector.ErrRateLimited, status)

		return false
	}
	if status != http.StatusOK {
		it.err = fmt.Errorf("freshdesk: list status=%d", status)

		return false
	}
	if decodeErr != nil {
		it.err = fmt.Errorf("freshdesk: decode list: %w", decodeErr)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, t := range tickets {
		ref := connector.DocumentRef{NamespaceID: "tickets", ID: strconv.FormatInt(t.ID, 10)}
		if ts, perr := time.Parse(time.RFC3339, t.UpdatedAt); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	switch {
	case !linkHasNext(linkHdr):
		it.done = true
	case len(tickets) < perPage:
		it.done = true
	default:
		it.nextPage++
	}

	return true
}

// ListDocuments enumerates Freshdesk tickets.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("freshdesk: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a Freshdesk ticket by ID.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("freshdesk: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v2/tickets/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: freshdesk: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("freshdesk: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var t ticketEntry
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("freshdesk: decode ticket: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, t.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, t.UpdatedAt)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}
	idStr := strconv.FormatInt(t.ID, 10)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "tickets", ID: idStr, UpdatedAt: updated},
		MIMEType:  "text/plain",
		Title:     t.Subject,
		Size:      int64(len(t.Description)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(t.Description)),
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls the tickets endpoint filtered by updated_since.
// Empty cursor returns "now" as the new cursor (bootstrap contract).
// Round-23 Devin Review fix: walk every `page=N` page rather than
// stopping after the first 100 tickets — previously a tenant with
// >100 updates between syncs would silently lose the trailing
// records until the next cycle. We rely on `len(tickets) < perPage`
// to detect the final page, and additionally honour Freshdesk's
// `Link: <url>; rel="next"` response header so the case where the
// total count is exactly N×perPage doesn't fire a final empty
// request.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("freshdesk: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("freshdesk: bad cursor %q: %w", cursor, err)
	}
	const perPage = 100
	newest := cursorTime
	var changes []connector.DocumentChange
	page := 1
	for {
		q := url.Values{}
		q.Set("updated_since", cursor)
		q.Set("order_by", "updated_at")
		q.Set("order_type", "asc")
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		resp, err := o.do(ctx, conn, http.MethodGet, "/api/v2/tickets?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		linkHdr := resp.Header.Get("Link")
		var tickets []ticketEntry
		decodeErr := json.NewDecoder(resp.Body).Decode(&tickets)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: freshdesk: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("freshdesk: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("freshdesk: decode delta: %w", decodeErr)
		}
		for _, t := range tickets {
			ts, perr := time.Parse(time.RFC3339, t.UpdatedAt)
			if perr != nil {
				continue
			}
			ts = ts.UTC()
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: strconv.FormatInt(t.ID, 10), UpdatedAt: ts}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
			if ts.After(newest) {
				newest = ts
			}
		}
		// Two EOF signals to avoid the wasted final empty page:
		// (1) Freshdesk sets `Link: <url>; rel="next"` only when
		// another page exists; (2) any short page is the last.
		if !linkHasNext(linkHdr) {
			break
		}
		if len(tickets) < perPage {
			break
		}
		page++
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// linkHasNext returns true when the RFC 8288 Link header advertises
// a `rel="next"` reference. Freshdesk uses this header to signal
// "another page exists", letting us short-circuit before the
// trailing empty request.
func linkHasNext(h string) bool {
	if h == "" {
		// Header absent — fall back to len(rows)<perPage probe.
		return true
	}

	return strings.Contains(h, `rel="next"`)
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("freshdesk: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHdr)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freshdesk: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
