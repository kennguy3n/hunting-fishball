// Package personio implements the Personio HR SourceConnector +
// DeltaSyncer against the Personio REST API at
// https://api.personio.de/v1. Authentication uses OAuth 2.0
// client credentials: client_id + client_secret are exchanged
// for a bearer token via /auth, which the connector then uses
// against /company/employees. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "client_id":     "...", // required
//	  "client_secret": "..."  // required
//	}
//
// The connector exposes a single synthetic namespace
// "employees" and uses `updated_from=<RFC3339>` for incremental
// synchronisation. Terminated employees (status="inactive")
// surface as `connector.ChangeDeleted`.
package personio

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

const (
	// Name is the registry-visible connector name.
	Name = "personio"

	defaultBaseURL = "https://api.personio.de/v1"

	// maxDeltaPages caps DeltaSync's inner pagination loop. With
	// the page size of 200 enforced below this bounds a single
	// sync at 10,000 changes, which is well above realistic
	// per-window churn for any tenant and prevents a runaway
	// loop if the server's pagination terminator is broken.
	// Mirrors google_workspace's identical guard.
	maxDeltaPages = 50
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(p *Connector) { p.httpClient = c } }

// WithBaseURL overrides the Personio base URL — used by tests.
func WithBaseURL(u string) Option { return func(p *Connector) { p.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	p := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

type connection struct {
	tenantID string
	sourceID string
	clientID string
	secret   string
	token    string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (p *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.ClientID == "" || creds.ClientSecret == "" {
		return fmt.Errorf("%w: client_id and client_secret required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect exchanges client_id + client_secret for a bearer token
// via POST /auth and stores it on the connection.
func (p *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := p.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, clientID: creds.ClientID, secret: creds.ClientSecret}
	tok, err := p.fetchToken(ctx, conn)
	if err != nil {
		return nil, err
	}
	conn.token = tok

	return conn, nil
}

func (p *Connector) fetchToken(ctx context.Context, conn *connection) (string, error) {
	q := url.Values{}
	q.Set("client_id", conn.clientID)
	q.Set("client_secret", conn.secret)
	target := strings.TrimRight(p.baseURL, "/") + "/auth?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return "", fmt.Errorf("personio: build auth request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("personio: auth: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: personio: auth status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("personio: auth status=%d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("personio: decode auth: %w", err)
	}
	tok := body.Data.Token
	if tok == "" {
		tok = body.Token
	}
	if tok == "" {
		return "", errors.New("personio: empty token from /auth")
	}

	return tok, nil
}

// ListNamespaces returns a single synthetic namespace "employees".
func (p *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "employees", Name: "Employees", Kind: "employees"}}, nil
}

type employeeIterator struct {
	p       *Connector
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

func (it *employeeIterator) Next(ctx context.Context) bool {
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

func (it *employeeIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *employeeIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *employeeIterator) Close() error { return nil }

func (it *employeeIterator) fetchPage(ctx context.Context) bool {
	it.first = true
	q := url.Values{}
	q.Set("limit", "200")
	q.Set("offset", fmt.Sprintf("%d", it.offset))
	resp, err := it.p.do(ctx, it.conn, http.MethodGet, "/company/employees?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: personio: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("personio: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Data []struct {
			Attributes struct {
				ID struct {
					Value int64 `json:"value"`
				} `json:"id"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("personio: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, e := range body.Data {
		id := fmt.Sprintf("%d", e.Attributes.ID.Value)
		if e.Attributes.ID.Value == 0 {
			continue
		}
		it.page = append(it.page, connector.DocumentRef{NamespaceID: it.ns.ID, ID: id})
	}
	it.offset += len(body.Data)
	if len(body.Data) == 0 {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}

	return true
}

// ListDocuments enumerates employees in the namespace.
func (p *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("personio: bad connection type")
	}

	return &employeeIterator{p: p, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single employee.
func (p *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("personio: bad connection type")
	}
	resp, err := p.do(ctx, conn, http.MethodGet, "/company/employees/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: personio: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("personio: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("personio: read body: %w", err)
	}
	var body struct {
		Data struct {
			Attributes struct {
				FirstName struct {
					Value string `json:"value"`
				} `json:"first_name"`
				LastName struct {
					Value string `json:"value"`
				} `json:"last_name"`
				Email struct {
					Value string `json:"value"`
				} `json:"email"`
				HireDate struct {
					Value string `json:"value"`
				} `json:"hire_date"`
				LastModifiedAt struct {
					Value string `json:"value"`
				} `json:"last_modified_at"`
			} `json:"attributes"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	a := body.Data.Attributes
	title := strings.TrimSpace(a.FirstName.Value + " " + a.LastName.Value)
	if title == "" {
		title = a.Email.Value
	}
	if title == "" {
		title = ref.ID
	}
	created, _ := time.Parse(time.RFC3339, a.HireDate.Value)
	updated, _ := time.Parse(time.RFC3339, a.LastModifiedAt.Value)
	if updated.IsZero() {
		updated = created
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    a.Email.Value,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": "employee"},
	}, nil
}

// Subscribe is unsupported — Personio webhook configuration is
// per-tenant in the admin console and uses HMAC signatures.
func (p *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (p *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses `updated_from=<RFC3339>`. Empty cursor returns
// "now" without backfilling. Employees with status="inactive"
// surface as `connector.ChangeDeleted`.
func (p *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("personio: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: revalidate auth + return "now".
		resp, err := p.do(ctx, conn, http.MethodGet, "/company/employees?limit=1", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: personio: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("personio: bootstrap status=%d", resp.StatusCode)
		}

		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	// Personio /company/employees pagination is offset-based.
	// Walk every page in the window before advancing the cursor
	// so we never silently drop changes past the first 200 rows.
	const pageLimit = 200
	var (
		changes []connector.DocumentChange
		offset  int
		page    int
	)
	for page = 0; page < maxDeltaPages; page++ {
		q := url.Values{}
		q.Set("updated_from", cursor)
		q.Set("limit", fmt.Sprintf("%d", pageLimit))
		q.Set("offset", fmt.Sprintf("%d", offset))
		resp, err := p.do(ctx, conn, http.MethodGet, "/company/employees?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("%w: personio: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, cursor, fmt.Errorf("personio: delta status=%d", resp.StatusCode)
		}
		var body struct {
			Data []struct {
				Attributes struct {
					ID struct {
						Value int64 `json:"value"`
					} `json:"id"`
					Status struct {
						Value string `json:"value"`
					} `json:"status"`
				} `json:"attributes"`
			} `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("personio: decode delta: %w", err)
		}
		for _, e := range body.Data {
			if e.Attributes.ID.Value == 0 {
				continue
			}
			kind := connector.ChangeUpserted
			if strings.EqualFold(e.Attributes.Status.Value, "inactive") {
				kind = connector.ChangeDeleted
			}
			changes = append(changes, connector.DocumentChange{
				Kind: kind,
				Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: fmt.Sprintf("%d", e.Attributes.ID.Value)},
			})
		}
		offset += len(body.Data)
		// A short page (or empty page) means we've drained the
		// updated_from window. Personio doesn't return a total,
		// so this is the only terminator.
		if len(body.Data) < pageLimit {
			break
		}
	}
	if page == maxDeltaPages {
		// We filled the entire safety budget without seeing a
		// short page. Surface the truncation instead of
		// silently advancing the cursor.
		return nil, cursor, fmt.Errorf("personio: delta exceeded %d pages (>%d changes); cursor not advanced", maxDeltaPages, maxDeltaPages*pageLimit)
	}

	return changes, time.Now().UTC().Format(time.RFC3339), nil
}

// do is the shared HTTP helper. Personio takes the bearer token
// directly in Authorization with no "Bearer " prefix in some
// endpoints; this connector uses the documented Authorization:
// Bearer <token> scheme on /company/* endpoints.
func (p *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(p.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("personio: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("personio: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Personio connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
