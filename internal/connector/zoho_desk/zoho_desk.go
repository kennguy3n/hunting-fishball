// Package zoho_desk implements the Zoho Desk SourceConnector +
// DeltaSyncer against the Zoho Desk REST API v1 at
// https://desk.zoho.<region>/api/v1. Authentication uses an OAuth
// access token via the `Authorization: Zoho-oauthtoken <token>`
// header.
//
// Credentials must be a JSON blob:
//
//	{
//	 "access_token": "...",   // required
//	 "org_id":       "..."    // required — the orgId header
//	}
//
// The connector exposes a single synthetic namespace "tickets".
// Delta is driven by `?modifiedTimeRange=<since>,<until>` paginated
// via the `from` and `limit` parameters.
package zoho_desk

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
const Name = "zoho_desk"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	OrgID       string `json:"org_id"`
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

// WithBaseURL pins the Zoho Desk base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://desk.zoho.com",
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
	orgID    string
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
	if creds.OrgID == "" {
		return fmt.Errorf("%w: org_id required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /api/v1/myinfo as a cheap auth check.
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
		orgID:    creds.OrgID,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/myinfo", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: zoho_desk: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zoho_desk: myinfo status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the single synthetic namespace "tickets".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "tickets", Name: "Tickets", Kind: "tickets"}}, nil
}

type ticketEntry struct {
	ID           string `json:"id"`
	Subject      string `json:"subject"`
	Description  string `json:"description"`
	ModifiedTime string `json:"modifiedTime"`
	CreatedTime  string `json:"createdTime"`
}

type listEnvelope struct {
	Data []ticketEntry `json:"data"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	from int
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
	const perPage = 100
	q := url.Values{}
	q.Set("from", strconv.Itoa(it.from))
	q.Set("limit", strconv.Itoa(perPage))
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/api/v1/tickets?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	status := resp.StatusCode
	var env listEnvelope
	// Zoho returns 204 No Content for an empty page; guard the
	// decode against the empty body.
	if status == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil && !errors.Is(err, io.EOF) {
			_ = resp.Body.Close()
			it.err = fmt.Errorf("zoho_desk: decode list: %w", err)

			return false
		}
	}
	_ = resp.Body.Close()
	switch {
	case status == http.StatusTooManyRequests:
		it.err = fmt.Errorf("%w: zoho_desk: list status=%d", connector.ErrRateLimited, status)

		return false
	case status == http.StatusNoContent:
		it.done = true

		return true
	case status != http.StatusOK:
		it.err = fmt.Errorf("zoho_desk: list status=%d", status)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, t := range env.Data {
		ref := connector.DocumentRef{NamespaceID: "tickets", ID: t.ID}
		if ts, perr := parseZohoTime(t.ModifiedTime); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	if len(env.Data) < perPage {
		it.done = true
	} else {
		it.from += len(env.Data)
	}

	return true
}

// ListDocuments enumerates Zoho Desk tickets.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("zoho_desk: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a Zoho Desk ticket by ID.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("zoho_desk: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/tickets/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: zoho_desk: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zoho_desk: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var t ticketEntry
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("zoho_desk: decode ticket: %w", err)
	}
	created, _ := parseZohoTime(t.CreatedTime)
	updated, _ := parseZohoTime(t.ModifiedTime)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "tickets", ID: t.ID, UpdatedAt: updated},
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

// DeltaSync polls /api/v1/tickets filtered by
// `modifiedTimeRange=<since>,<until>`. Empty cursor returns "now"
// without backfilling.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("zoho_desk: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("zoho_desk: bad cursor %q: %w", cursor, err)
	}
	until := time.Now().UTC().Format(time.RFC3339)
	const perPage = 100
	newest := cursorTime
	var changes []connector.DocumentChange
	from := 0
	for {
		q := url.Values{}
		q.Set("modifiedTimeRange", cursor+","+until)
		q.Set("from", strconv.Itoa(from))
		q.Set("limit", strconv.Itoa(perPage))
		q.Set("sortBy", "modifiedTime")
		resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/tickets?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		var env listEnvelope
		if status == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil && !errors.Is(err, io.EOF) {
				_ = resp.Body.Close()

				return nil, "", fmt.Errorf("zoho_desk: decode delta: %w", err)
			}
		}
		_ = resp.Body.Close()
		switch {
		case status == http.StatusTooManyRequests:
			return nil, "", fmt.Errorf("%w: zoho_desk: status=%d", connector.ErrRateLimited, status)
		case status == http.StatusNoContent:
			return changes, newest.UTC().Format(time.RFC3339), nil
		case status != http.StatusOK:
			return nil, cursor, fmt.Errorf("zoho_desk: delta status=%d", status)
		}
		for _, t := range env.Data {
			ts, perr := parseZohoTime(t.ModifiedTime)
			if perr != nil {
				continue
			}
			ts = ts.UTC()
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: t.ID, UpdatedAt: ts}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
			if ts.After(newest) {
				newest = ts
			}
		}
		if len(env.Data) < perPage {
			break
		}
		from += len(env.Data)
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// parseZohoTime accepts both the canonical RFC3339 layout and Zoho's
// occasional `YYYY-MM-DDTHH:MM:SS.000Z` millisecond variant.
func parseZohoTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("zoho_desk: empty time")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}

	return time.Parse("2006-01-02T15:04:05.000Z07:00", s)
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("zoho_desk: build request: %w", err)
	}
	req.Header.Set("Authorization", "Zoho-oauthtoken "+conn.token)
	req.Header.Set("orgId", conn.orgID)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zoho_desk: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
