// Package bamboohr implements the BambooHR SourceConnector +
// DeltaSyncer against the BambooHR REST API at
// https://api.bamboohr.com/api/gateway.php/<subdomain>/v1.
// Authentication uses basic auth with the api_key as the
// username and "x" as the password (per BambooHR docs). We use
// stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_key":   "abcd1234...", // required
//	  "subdomain": "acme"          // required
//	}
//
// The connector exposes a single synthetic namespace
// "employees" and uses `/v1/employees/changed?since=<ISO8601>`
// for incremental synchronisation.
package bamboohr

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
const Name = "bamboohr"

// isRateLimited returns true for HTTP statuses BambooHR uses to
// signal throttling. The API documents 429 Too Many Requests,
// but in practice many endpoints surface 503 Service Unavailable
// as the primary back-pressure signal (with a Retry-After
// header). We wrap both into connector.ErrRateLimited so the
// adaptive_rate_limiter backs off on either.
func isRateLimited(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode == http.StatusServiceUnavailable
}

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIKey    string `json:"api_key"`
	Subdomain string `json:"subdomain"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(b *Connector) { b.httpClient = c } }

// WithBaseURL pins the BambooHR base URL — used by tests.
func WithBaseURL(u string) Option { return func(b *Connector) { b.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	b := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(b)
	}

	return b
}

type connection struct {
	tenantID string
	sourceID string
	key      string
	base     string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (b *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.APIKey == "" {
		return fmt.Errorf("%w: api_key required", connector.ErrInvalidConfig)
	}
	if creds.Subdomain == "" && b.baseURL == "" {
		return fmt.Errorf("%w: subdomain required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /v1/employees/directory as a cheap auth check.
func (b *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := b.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := b.baseURL
	if base == "" {
		base = "https://api.bamboohr.com/api/gateway.php/" + creds.Subdomain
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, key: creds.APIKey, base: base}
	resp, err := b.do(ctx, conn, http.MethodGet, "/v1/employees/directory", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if isRateLimited(resp.StatusCode) {
		return nil, fmt.Errorf("%w: bamboohr: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bamboohr: /directory status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns a single synthetic namespace "employees".
func (b *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "employees", Name: "Employees", Kind: "employees"}}, nil
}

type employeeIterator struct {
	b       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	loaded  bool
	err     error
}

func (it *employeeIterator) Next(ctx context.Context) bool {
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

func (it *employeeIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *employeeIterator) Err() error {
	if it.err == nil && it.loaded {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *employeeIterator) Close() error { return nil }

func (it *employeeIterator) fetch(ctx context.Context) bool {
	resp, err := it.b.do(ctx, it.conn, http.MethodGet, "/v1/employees/directory", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if isRateLimited(resp.StatusCode) {
		it.err = fmt.Errorf("%w: bamboohr: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("bamboohr: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Employees []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			WorkEmail   string `json:"workEmail"`
		} `json:"employees"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("bamboohr: decode list: %w", err)

		return false
	}
	for _, e := range body.Employees {
		it.page = append(it.page, connector.DocumentRef{NamespaceID: it.ns.ID, ID: e.ID})
	}

	return true
}

// ListDocuments enumerates employees from the directory endpoint.
func (b *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bamboohr: bad connection type")
	}

	return &employeeIterator{b: b, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single employee by ID with common fields.
func (b *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bamboohr: bad connection type")
	}
	q := url.Values{}
	q.Set("fields", "displayName,workEmail,jobTitle,department,hireDate,terminationDate,status")
	resp, err := b.do(ctx, conn, http.MethodGet, "/v1/employees/"+url.PathEscape(ref.ID)+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if isRateLimited(resp.StatusCode) {
		return nil, fmt.Errorf("%w: bamboohr: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bamboohr: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bamboohr: read body: %w", err)
	}
	var body struct {
		ID              string `json:"id"`
		DisplayName     string `json:"displayName"`
		WorkEmail       string `json:"workEmail"`
		JobTitle        string `json:"jobTitle"`
		HireDate        string `json:"hireDate"`
		TerminationDate string `json:"terminationDate"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.DisplayName
	if title == "" {
		title = body.WorkEmail
	}
	if title == "" {
		title = body.ID
	}
	created, _ := time.Parse("2006-01-02", body.HireDate)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.WorkEmail,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: created,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": "employee"},
	}, nil
}

// Subscribe is unsupported — BambooHR webhooks require per-tenant
// configuration in the admin console.
func (b *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (b *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses `/v1/employees/changed?since=<RFC3339>`. Empty
// cursor returns "now" as the cursor without backfilling.
// Employees whose returned `action` is `Deleted` are mapped to
// `connector.ChangeDeleted`.
func (b *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("bamboohr: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: a cheap auth-revalidation hit to /directory
		// keeps the registry honest about returning "now" without
		// emitting backfill changes.
		resp, err := b.do(ctx, conn, http.MethodGet, "/v1/employees/directory", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if isRateLimited(resp.StatusCode) {
			return nil, "", fmt.Errorf("%w: bamboohr: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("bamboohr: bootstrap status=%d", resp.StatusCode)
		}

		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	q := url.Values{}
	q.Set("since", cursor)
	resp, err := b.do(ctx, conn, http.MethodGet, "/v1/employees/changed?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if isRateLimited(resp.StatusCode) {
		return nil, "", fmt.Errorf("%w: bamboohr: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("bamboohr: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Employees map[string]struct {
			Action      string `json:"action"`
			LastChanged string `json:"lastChanged"`
		} `json:"employees"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("bamboohr: decode delta: %w", err)
	}
	changes := make([]connector.DocumentChange, 0, len(body.Employees))
	for id, e := range body.Employees {
		kind := connector.ChangeUpserted
		if strings.EqualFold(e.Action, "Deleted") {
			kind = connector.ChangeDeleted
		}
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: id},
		})
	}

	return changes, time.Now().UTC().Format(time.RFC3339), nil
}

// do is the shared HTTP helper. BambooHR uses basic auth with the
// api_key as username and "x" as the password.
func (b *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.base, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("bamboohr: build request: %w", err)
	}
	req.SetBasicAuth(conn.key, "x")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bamboohr: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the BambooHR connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
