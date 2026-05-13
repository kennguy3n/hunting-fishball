// Package airtable implements the Airtable SourceConnector +
// DeltaSyncer against the Airtable REST API at
// https://api.airtable.com/v0/{baseId}/{tableIdOrName}.
// Authentication uses a personal access token or OAuth bearer.
//
// Credentials must be a JSON blob:
//
//	{
//	 "access_token": "...",    // required (PAT or OAuth)
//	 "base_id":      "appXXX", // required
//	 "table":        "Tasks"   // required (id or name)
//	}
//
// The connector exposes the configured base/table as the single
// namespace. Delta uses Airtable's `filterByFormula` with
// `LAST_MODIFIED_TIME()>'<ISO8601>'` and the standard `offset`
// pagination token.
package airtable

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
const Name = "airtable"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	BaseID      string `json:"base_id"`
	Table       string `json:"table"`
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

// WithBaseURL pins the Airtable base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.airtable.com"}
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
	baseID      string
	table       string
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
	if creds.BaseID == "" {
		return fmt.Errorf("%w: base_id required", connector.ErrInvalidConfig)
	}
	if creds.Table == "" {
		return fmt.Errorf("%w: table required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues a cheap GET against the table (limit=1) as auth check.
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
		baseID:      creds.BaseID,
		table:       creds.Table,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/v0/"+url.PathEscape(conn.baseID)+"/"+url.PathEscape(conn.table)+"?maxRecords=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: airtable: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airtable: ping status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the configured base/table as the only namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("airtable: bad connection type")
	}

	return []connector.Namespace{{ID: conn.baseID + "/" + conn.table, Name: conn.table, Kind: "table"}}, nil
}

type recordEntry struct {
	ID          string                 `json:"id"`
	CreatedTime string                 `json:"createdTime"`
	Fields      map[string]interface{} `json:"fields"`
}

type listResponse struct {
	Records []recordEntry `json:"records"`
	Offset  string        `json:"offset"`
}

type docIterator struct {
	o      *Connector
	conn   *connection
	ns     connector.Namespace
	offset string
	page   []connector.DocumentRef
	idx    int
	more   bool
	err    error
}

func (it *docIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	if it.idx >= len(it.page) {
		if !it.more && it.page != nil {
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
	if it.err == nil && !it.more {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *docIterator) Close() error { return nil }

func (it *docIterator) fetch(ctx context.Context) bool {
	q := url.Values{}
	q.Set("pageSize", "100")
	if it.offset != "" {
		q.Set("offset", it.offset)
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/v0/"+url.PathEscape(it.conn.baseID)+"/"+url.PathEscape(it.conn.table)+"?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: airtable: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("airtable: list status=%d", resp.StatusCode)

		return false
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("airtable: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, r := range body.Records {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: r.ID}
		if ts, perr := time.Parse(time.RFC3339, r.CreatedTime); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.offset = body.Offset
	it.more = body.Offset != ""

	return true
}

// ListDocuments enumerates Airtable records in the table.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("airtable: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns, more: true}, nil
}

// FetchDocument loads a single record by id.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("airtable: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/v0/"+url.PathEscape(conn.baseID)+"/"+url.PathEscape(conn.table)+"/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: airtable: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("airtable: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var rec recordEntry
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("airtable: decode record: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, rec.CreatedTime)
	raw, _ := json.Marshal(rec.Fields)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: conn.baseID + "/" + conn.table, ID: rec.ID, UpdatedAt: created},
		MIMEType:  "application/json",
		Title:     rec.ID,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: created,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses filterByFormula on LAST_MODIFIED_TIME() with an
// RFC 3339 cursor. Empty cursor returns "now" without backfilling.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("airtable: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("airtable: bad cursor %q: %w", cursor, err)
	}
	q := url.Values{}
	q.Set("filterByFormula", "IS_AFTER(LAST_MODIFIED_TIME(),'"+cursor+"')")
	q.Set("pageSize", "100")
	resp, err := o.do(ctx, conn, http.MethodGet, "/v0/"+url.PathEscape(conn.baseID)+"/"+url.PathEscape(conn.table)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: airtable: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("airtable: delta status=%d", resp.StatusCode)
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("airtable: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, r := range body.Records {
		ts, _ := time.Parse(time.RFC3339, r.CreatedTime)
		ts = ts.UTC()
		ref := connector.DocumentRef{NamespaceID: ns.ID, ID: r.ID, UpdatedAt: ts}
		changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		if ts.After(newest) {
			newest = ts
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("airtable: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("airtable: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
