// Package pipedrive implements the Pipedrive SourceConnector +
// DeltaSyncer against the Pipedrive REST v1 API at
// https://<domain>.pipedrive.com/api/v1. Authentication uses an
// `api_token` query parameter. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_token": "...",                    // required
//	  "company_domain": "acme"               // required
//	}
package pipedrive

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

const (
	// Name is the registry-visible connector name.
	Name = "pipedrive"

	defaultPageSize = 100
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIToken      string `json:"api_token"`
	CompanyDomain string `json:"company_domain"`
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

// WithBaseURL overrides the Pipedrive API base URL — used by tests.
func WithBaseURL(u string) Option { return func(p *Connector) { p.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	p := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	baseURL  string
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
	if creds.APIToken == "" {
		return fmt.Errorf("%w: api_token required", connector.ErrInvalidConfig)
	}
	if p.baseURL == "" && creds.CompanyDomain == "" {
		return fmt.Errorf("%w: company_domain required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /users/me as the auth check.
func (p *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := p.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := p.baseURL
	if base == "" {
		base = "https://" + creds.CompanyDomain + ".pipedrive.com/api/v1"
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.APIToken, baseURL: base}
	resp, err := p.do(ctx, conn, http.MethodGet, "/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: pipedrive: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pipedrive: /users/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the three top-level Pipedrive object
// types — deals, persons, activities — as namespaces.
func (p *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{
		{ID: "deals", Name: "Deals", Kind: "deals"},
		{ID: "persons", Name: "Persons", Kind: "persons"},
		{ID: "activities", Name: "Activities", Kind: "activities"},
	}, nil
}

// objectIterator paginates over a top-level resource.
type objectIterator struct {
	p       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	start   int
	err     error
	done    bool
}

func (it *objectIterator) Next(ctx context.Context) bool {
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

func (it *objectIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *objectIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *objectIterator) Close() error { return nil }

func (it *objectIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("start", strconv.Itoa(it.start))
	q.Set("limit", strconv.Itoa(defaultPageSize))
	resp, err := it.p.do(ctx, it.conn, http.MethodGet, "/"+it.ns.ID+"?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: pipedrive: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("pipedrive: list %s status=%d", it.ns.ID, resp.StatusCode)

		return false
	}
	var body struct {
		Success bool `json:"success"`
		Data    []struct {
			ID         int    `json:"id"`
			UpdateTime string `json:"update_time"`
		} `json:"data"`
		AdditionalData struct {
			Pagination struct {
				MoreItemsInCollection bool `json:"more_items_in_collection"`
				NextStart             int  `json:"next_start"`
			} `json:"pagination"`
		} `json:"additional_data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("pipedrive: decode: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, d := range body.Data {
		updated, _ := time.Parse("2006-01-02 15:04:05", d.UpdateTime)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          strconv.Itoa(d.ID),
			ETag:        d.UpdateTime,
			UpdatedAt:   updated,
		})
	}
	if !body.AdditionalData.Pagination.MoreItemsInCollection {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.start = body.AdditionalData.Pagination.NextStart
	}

	return true
}

// ListDocuments enumerates objects in the namespace.
func (p *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("pipedrive: bad connection type")
	}

	return &objectIterator{p: p, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single record by ID.
func (p *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("pipedrive: bad connection type")
	}
	resp, err := p.do(ctx, conn, http.MethodGet, "/"+ref.NamespaceID+"/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: pipedrive: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pipedrive: fetch %s/%s status=%d", ref.NamespaceID, ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pipedrive: read body: %w", err)
	}
	var body struct {
		Data struct {
			ID         int    `json:"id"`
			Title      string `json:"title"`
			Name       string `json:"name"`
			Subject    string `json:"subject"`
			AddTime    string `json:"add_time"`
			UpdateTime string `json:"update_time"`
			OwnerName  string `json:"owner_name"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.Data.Title
	if title == "" {
		title = body.Data.Name
	}
	if title == "" {
		title = body.Data.Subject
	}
	created, _ := time.Parse("2006-01-02 15:04:05", body.Data.AddTime)
	updated, _ := time.Parse("2006-01-02 15:04:05", body.Data.UpdateTime)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.Data.OwnerName,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Pipedrive supports webhooks
// (HandleWebhook in a later round).
func (p *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (p *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses /<kind>?since_timestamp=YYYY-MM-DD HH:MM:SS in UTC
// to fetch records updated after the cursor. Empty cursor on the
// first call bootstraps from "now" without backfilling history.
func (p *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("pipedrive: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: fetch the newest record by update_time (sort
		// desc, limit 1) and return that timestamp.
		q := url.Values{}
		q.Set("limit", "1")
		q.Set("sort", "update_time DESC")
		resp, err := p.do(ctx, conn, http.MethodGet, "/"+ns.ID+"?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: pipedrive: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("pipedrive: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Data []struct {
				UpdateTime string `json:"update_time"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("pipedrive: decode bootstrap: %w", err)
		}
		newCursor := time.Now().UTC().Format("2006-01-02 15:04:05")
		if len(body.Data) > 0 && body.Data[0].UpdateTime != "" {
			newCursor = body.Data[0].UpdateTime
		}

		return nil, newCursor, nil
	}
	q := url.Values{}
	q.Set("since_timestamp", cursor)
	q.Set("sort", "update_time ASC")
	q.Set("limit", strconv.Itoa(defaultPageSize))
	resp, err := p.do(ctx, conn, http.MethodGet, "/recents?"+q.Encode(), nil)
	// /recents accepts the unified filter — but we also fall back
	// to per-kind list with sort below if the upstream returns a
	// non-200 that isn't 429.
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: pipedrive: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("pipedrive: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			Item string          `json:"item"`
			Data json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("pipedrive: decode delta: %w", err)
	}
	singular := ns.ID
	if trimmed := strings.TrimSuffix(ns.ID, "ies"); trimmed != ns.ID {
		singular = trimmed + "y"
	} else if trimmed := strings.TrimSuffix(ns.ID, "s"); trimmed != ns.ID {
		singular = trimmed
	}
	newCursor := cursor
	var changes []connector.DocumentChange
	for _, d := range body.Data {
		if d.Item != singular && d.Item != ns.ID {
			continue
		}
		var item struct {
			ID         int    `json:"id"`
			UpdateTime string `json:"update_time"`
		}
		if err := json.Unmarshal(d.Data, &item); err != nil {
			continue
		}
		updated, _ := time.Parse("2006-01-02 15:04:05", item.UpdateTime)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          strconv.Itoa(item.ID),
				ETag:        item.UpdateTime,
				UpdatedAt:   updated,
			},
		})
		if item.UpdateTime > newCursor {
			newCursor = item.UpdateTime
		}
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper. Pipedrive authenticates via an
// `api_token` query parameter (its REST v1 API does not accept
// Authorization headers for personal API tokens). The token is
// attached via req.URL.Query() AFTER request construction so the
// raw URL we format into errors does not carry it, and any error
// returned by http.Client.Do — which contains *url.Error.URL with
// the full query string — has the token redacted before wrapping.
func (p *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	safeTarget := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, safeTarget, body)
	if err != nil {
		return nil, fmt.Errorf("pipedrive: build request: %w", err)
	}
	q := req.URL.Query()
	q.Set("api_token", conn.token)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pipedrive: %s %s: %s", method, safeTarget, redactToken(err.Error(), conn.token))
	}

	return resp, nil
}

// redactToken removes the api_token query parameter value from any
// stringified error path that Go's net/http may embed.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "REDACTED")
	s = strings.ReplaceAll(s, url.QueryEscape(token), "REDACTED")

	return s
}

// Register registers the Pipedrive connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
