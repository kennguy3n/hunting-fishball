// Package servicenow implements the ServiceNow Table API
// SourceConnector + DeltaSyncer against the ServiceNow REST API at
// https://<instance>.service-now.com/api/now/table/<table>.
// Authentication uses either HTTP basic auth (username + password)
// or an OAuth bearer token; we expose both via a single Credentials
// struct.
//
// Credentials must be a JSON blob:
//
//	{
//	 "instance":     "acme",       // required
//	 "username":     "svc",        // either basic auth pair...
//	 "password":     "...",
//	 "access_token": "..."         // ...or oauth bearer
//	}
//
// Settings carries the table name (default "incident") under the
// key "table". Delta uses the standard
// `sysparm_query=sys_updated_on>'<datetime>'` filter with
// `sysparm_offset` pagination.
package servicenow

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
const Name = "servicenow"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Instance    string `json:"instance"`
	Username    string `json:"username"`
	Password    string `json:"password"`
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

// WithBaseURL pins the ServiceNow base URL — used by tests.
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
	table    string
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
	if creds.Instance == "" && o.baseURL == "" {
		return fmt.Errorf("%w: instance required", connector.ErrInvalidConfig)
	}
	if creds.AccessToken == "" && (creds.Username == "" || creds.Password == "") {
		return fmt.Errorf("%w: username+password or access_token required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues a small Table API query against the configured
// table as a cheap auth check.
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
		base = "https://" + creds.Instance + ".service-now.com"
	}
	auth := ""
	if creds.AccessToken != "" {
		auth = "Bearer " + creds.AccessToken
	} else {
		raw := creds.Username + ":" + creds.Password
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}
	table := "incident"
	if cfg.Settings != nil {
		if v, ok := cfg.Settings["table"].(string); ok && v != "" {
			table = v
		}
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, baseURL: base, authHdr: auth, table: table}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/now/table/"+url.PathEscape(table)+"?sysparm_limit=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: servicenow: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("servicenow: table %s status=%d", table, resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the configured table as the only namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("servicenow: bad connection type")
	}

	return []connector.Namespace{{ID: conn.table, Name: conn.table, Kind: "table"}}, nil
}

type recordEntry struct {
	SysID        string `json:"sys_id"`
	Number       string `json:"number"`
	ShortDesc    string `json:"short_description"`
	Description  string `json:"description"`
	SysUpdatedOn string `json:"sys_updated_on"`
	SysCreatedOn string `json:"sys_created_on"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// offset is the next ServiceNow `sysparm_offset` value to send.
	// We advance by `sysparm_limit` after each successful page until
	// the API returns fewer than that many records, at which point we
	// mark the iterator done. Round-22 pagination fix.
	offset int
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
	// Round-22 pagination fix: walk all pages via sysparm_offset.
	const limit = 100
	q := url.Values{}
	q.Set("sysparm_limit", strconv.Itoa(limit))
	q.Set("sysparm_offset", strconv.Itoa(it.offset))
	path := "/api/now/table/" + url.PathEscape(it.conn.table) + "?" + q.Encode()
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: servicenow: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("servicenow: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Result []recordEntry `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("servicenow: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, r := range body.Result {
		ref := connector.DocumentRef{NamespaceID: it.conn.table, ID: r.SysID}
		if ts, perr := time.Parse("2006-01-02 15:04:05", r.SysUpdatedOn); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	if len(body.Result) < limit {
		it.done = true
	} else {
		it.offset += len(body.Result)
	}

	return true
}

// ListDocuments enumerates ServiceNow records from the configured table.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("servicenow: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a single ServiceNow record by sys_id.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("servicenow: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/now/table/"+url.PathEscape(conn.table)+"/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: servicenow: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("servicenow: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var body struct {
		Result recordEntry `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("servicenow: decode record: %w", err)
	}
	created, _ := time.Parse("2006-01-02 15:04:05", body.Result.SysCreatedOn)
	updated, _ := time.Parse("2006-01-02 15:04:05", body.Result.SysUpdatedOn)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}
	title := body.Result.ShortDesc
	if title == "" {
		title = body.Result.Number
	}
	descr := body.Result.Description

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: conn.table, ID: body.Result.SysID, UpdatedAt: updated},
		MIMEType:  "text/plain",
		Title:     title,
		Size:      int64(len(descr)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(descr)),
		Metadata:  map[string]string{"number": body.Result.Number},
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync filters by sys_updated_on with offset pagination. Empty
// cursor returns the current "now" timestamp without backfilling.
// Round-23 Devin Review fix: walk every sysparm_offset page rather
// than capping at the first 100 records. EOF is detected via the
// short-page probe (len(result) < sysparm_limit) so we never fire
// a wasted trailing empty request.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("servicenow: bad connection type")
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if cursor == "" {
		return nil, now, nil
	}
	const limit = 100
	var changes []connector.DocumentChange
	newest := cursor
	offset := 0
	for {
		q := url.Values{}
		q.Set("sysparm_query", "sys_updated_on>"+cursor+"^ORDERBYsys_updated_on")
		q.Set("sysparm_limit", strconv.Itoa(limit))
		q.Set("sysparm_offset", strconv.Itoa(offset))
		resp, err := o.do(ctx, conn, http.MethodGet, "/api/now/table/"+url.PathEscape(conn.table)+"?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		var body struct {
			Result []recordEntry `json:"result"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: servicenow: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("servicenow: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("servicenow: decode delta: %w", decodeErr)
		}
		for _, r := range body.Result {
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: r.SysID}
			if r.SysUpdatedOn != "" {
				ref.UpdatedAt = parseSNTime(r.SysUpdatedOn)
				if r.SysUpdatedOn > newest {
					newest = r.SysUpdatedOn
				}
			}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		}
		if len(body.Result) < limit {
			break
		}
		offset += len(body.Result)
	}

	return changes, newest, nil
}

func parseSNTime(v string) time.Time {
	ts, err := time.Parse("2006-01-02 15:04:05", v)
	if err != nil {
		return time.Time{}
	}

	return ts.UTC()
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("servicenow: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHdr)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("servicenow: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
