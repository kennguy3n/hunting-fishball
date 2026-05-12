// Package workday implements the Workday HR SourceConnector +
// DeltaSyncer against the Workday REST API. Authentication uses
// either an OAuth 2.0 bearer access token or an API key passed
// via the standard Authorization header. We use stdlib net/http
// only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "...", // required (Bearer)
//	  "tenant_url":   "https://wd5-impl-services1.workday.com/ccx/api/v1/<tenant>" // required
//	}
//
// The connector exposes a single synthetic namespace "workers"
// and uses the `Updated_From=<RFC3339>` query parameter for
// incremental synchronisation. Terminated workers surface as
// `connector.ChangeDeleted`.
package workday

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
const Name = "workday"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	TenantURL   string `json:"tenant_url"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(w *Connector) { w.httpClient = c } }

// WithBaseURL pins the tenant URL — used by tests. Production
// callers should pass the tenant URL in credentials.
func WithBaseURL(u string) Option { return func(w *Connector) { w.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	w := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(w)
	}

	return w
}

type connection struct {
	tenantID  string
	sourceID  string
	token     string
	tenantURL string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (w *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.TenantURL == "" && w.baseURL == "" {
		return fmt.Errorf("%w: tenant_url required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect probes /workers?limit=1 as a cheap auth check.
func (w *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := w.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	tenantURL := creds.TenantURL
	if w.baseURL != "" {
		tenantURL = w.baseURL
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken, tenantURL: tenantURL}
	resp, err := w.do(ctx, conn, http.MethodGet, "/workers?limit=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: workday: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workday: /workers status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns a single synthetic namespace "workers".
func (w *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "workers", Name: "Workers", Kind: "workers"}}, nil
}

type workerIterator struct {
	w       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	offset  int
	first   bool
	err     error
	done    bool
}

func (it *workerIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	if it.pageIdx >= len(it.page) {
		if it.done {
			return false
		}
		if !it.fetchPage(ctx) {
			return false
		}
	}
	if it.pageIdx < len(it.page) {
		it.pageIdx++

		return true
	}

	return false
}

func (it *workerIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *workerIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *workerIterator) Close() error { return nil }

func (it *workerIterator) fetchPage(ctx context.Context) bool {
	if !it.first {
		it.first = true
	}
	q := url.Values{}
	q.Set("limit", "100")
	q.Set("offset", fmt.Sprintf("%d", it.offset))
	resp, err := it.w.do(ctx, it.conn, http.MethodGet, "/workers?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: workday: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("workday: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Data []struct {
			ID       string `json:"id"`
			Active   *bool  `json:"active"`
			JobTitle string `json:"jobTitle"`
		} `json:"data"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("workday: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, wk := range body.Data {
		it.page = append(it.page, connector.DocumentRef{NamespaceID: it.ns.ID, ID: wk.ID})
	}
	it.offset += len(body.Data)
	if len(body.Data) == 0 || it.offset >= body.Total {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}

	return true
}

// ListDocuments enumerates workers in the namespace.
func (w *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("workday: bad connection type")
	}

	return &workerIterator{w: w, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single worker by ID.
func (w *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("workday: bad connection type")
	}
	resp, err := w.do(ctx, conn, http.MethodGet, "/workers/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: workday: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workday: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("workday: read body: %w", err)
	}
	var body struct {
		ID            string `json:"id"`
		JobTitle      string `json:"jobTitle"`
		PrimaryEmail  string `json:"primaryEmail"`
		FullName      string `json:"fullName"`
		HireDate      string `json:"hireDate"`
		LastModified  string `json:"lastModifiedDateTime"`
		TerminationOn string `json:"terminationDate"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.FullName
	if title == "" {
		title = body.JobTitle
	}
	if title == "" {
		title = body.ID
	}
	created, _ := time.Parse(time.RFC3339, body.HireDate)
	updated, _ := time.Parse(time.RFC3339, body.LastModified)
	if updated.IsZero() {
		updated = created
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.PrimaryEmail,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": "worker"},
	}, nil
}

// Subscribe is unsupported — Workday uses webhook
// configuration via Studio integration that's out of scope here.
func (w *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (w *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses the `Updated_From=<RFC3339>` filter. Empty
// cursor on the first call returns the newest worker's
// lastModifiedDateTime (DESC + limit=1) without backfilling
// history. Terminated workers surface as `ChangeDeleted`.
func (w *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("workday: bad connection type")
	}
	if cursor == "" {
		q := url.Values{}
		q.Set("limit", "1")
		q.Set("order", "-lastModifiedDateTime")
		resp, err := w.do(ctx, conn, http.MethodGet, "/workers?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: workday: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("workday: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Data []struct {
				LastModified string `json:"lastModifiedDateTime"`
			} `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		ts := time.Now().UTC().Format(time.RFC3339)
		if len(body.Data) > 0 && body.Data[0].LastModified != "" {
			if t, err := time.Parse(time.RFC3339, body.Data[0].LastModified); err == nil {
				ts = t.UTC().Format(time.RFC3339)
			}
		}

		return nil, ts, nil
	}
	q := url.Values{}
	q.Set("Updated_From", cursor)
	q.Set("limit", "200")
	resp, err := w.do(ctx, conn, http.MethodGet, "/workers?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: workday: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("workday: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			Active        *bool  `json:"active"`
			LastModified  string `json:"lastModifiedDateTime"`
			TerminationOn string `json:"terminationDate"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("workday: decode delta: %w", err)
	}
	newestSeen := cursor
	changes := make([]connector.DocumentChange, 0, len(body.Data))
	for _, wk := range body.Data {
		kind := connector.ChangeUpserted
		if (wk.Active != nil && !*wk.Active) || wk.TerminationOn != "" {
			kind = connector.ChangeDeleted
		}
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: wk.ID},
		})
		if wk.LastModified > newestSeen {
			newestSeen = wk.LastModified
		}
	}

	return changes, newestSeen, nil
}

// do is the shared HTTP helper.
func (w *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.tenantURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("workday: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("workday: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Workday connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
