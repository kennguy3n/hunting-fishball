// Package okta implements the Okta SourceConnector + DeltaSyncer
// against the Okta Management API at
// https://<org>.okta.com/api/v1. Authentication uses an SSWS
// (Static Session) API token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_token": "00abc...",            // required
//	  "org_url":   "https://acme.okta.com" // required
//	}
package okta

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
	Name = "okta"

	defaultPageSize = 200
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIToken string `json:"api_token"`
	OrgURL   string `json:"org_url"`
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

// WithBaseURL overrides the Okta org URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
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
	if creds.APIToken == "" {
		return fmt.Errorf("%w: api_token required", connector.ErrInvalidConfig)
	}
	if o.baseURL == "" && creds.OrgURL == "" {
		return fmt.Errorf("%w: org_url required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /api/v1/users/me as the auth check.
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
		base = creds.OrgURL
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.APIToken, baseURL: base}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: okta: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("okta: /users/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns two synthetic namespaces: users + groups.
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{
		{ID: "users", Name: "Users", Kind: "users"},
		{ID: "groups", Name: "Groups", Kind: "groups"},
	}, nil
}

// principalIterator paginates over users or groups using the
// Link: next URL header that Okta returns for pagination.
type principalIterator struct {
	o       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	nextURL string
	first   bool
	err     error
	done    bool
}

func (it *principalIterator) Next(ctx context.Context) bool {
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

func (it *principalIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *principalIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *principalIterator) Close() error { return nil }

func (it *principalIterator) fetchPage(ctx context.Context) bool {
	var resp *http.Response
	var err error
	if !it.first {
		it.first = true
		path := "/api/v1/" + it.ns.ID
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", defaultPageSize))
		resp, err = it.o.do(ctx, it.conn, http.MethodGet, path+"?"+q.Encode(), nil)
	} else if it.nextURL != "" {
		resp, err = it.o.doAbsolute(ctx, it.conn, http.MethodGet, it.nextURL, nil)
	} else {
		it.done = true

		return false
	}
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: okta: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("okta: list %s status=%d", it.ns.ID, resp.StatusCode)

		return false
	}
	var body []struct {
		ID          string `json:"id"`
		LastUpdated string `json:"lastUpdated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("okta: decode: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, p := range body {
		updated, _ := time.Parse(time.RFC3339, p.LastUpdated)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          p.ID,
			ETag:        p.LastUpdated,
			UpdatedAt:   updated,
		})
	}
	it.nextURL = nextLink(resp.Header.Get("Link"))
	if it.nextURL == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}

	return true
}

// nextLink parses Okta's RFC 5988 Link header and returns the
// rel=next URL if present, "" otherwise.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		u := strings.Trim(segs[0], "<>")
		for _, s := range segs[1:] {
			if strings.Contains(strings.ToLower(s), `rel="next"`) {
				return u
			}
		}
	}

	return ""
}

// ListDocuments enumerates users / groups in the namespace.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("okta: bad connection type")
	}

	return &principalIterator{o: o, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single user or group by ID.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("okta: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/"+ref.NamespaceID+"/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: okta: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("okta: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("okta: read body: %w", err)
	}
	var body struct {
		ID          string `json:"id"`
		Created     string `json:"created"`
		LastUpdated string `json:"lastUpdated"`
		Profile     struct {
			Login     string `json:"login"`
			Name      string `json:"name"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
			Email     string `json:"email"`
		} `json:"profile"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.Profile.Login
	if title == "" {
		title = body.Profile.Name
	}
	if title == "" && (body.Profile.FirstName != "" || body.Profile.LastName != "") {
		title = strings.TrimSpace(body.Profile.FirstName + " " + body.Profile.LastName)
	}
	if title == "" {
		title = body.ID
	}
	created, _ := time.Parse(time.RFC3339, body.Created)
	updated, _ := time.Parse(time.RFC3339, body.LastUpdated)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.Profile.Email,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Okta supports Event Hooks
// (HandleWebhook in a later round).
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync filters by `lastUpdated gt "<cursor>"`. Empty cursor
// on the first call returns the newest principal's lastUpdated
// without backfilling history.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("okta: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: fetch the newest principal (sortBy
		// lastUpdated desc, limit 1) and return its lastUpdated.
		q := url.Values{}
		q.Set("limit", "1")
		q.Set("sortBy", "lastUpdated")
		q.Set("sortOrder", "desc")
		resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/"+ns.ID+"?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: okta: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("okta: bootstrap status=%d", resp.StatusCode)
		}
		var body []struct {
			LastUpdated string `json:"lastUpdated"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("okta: decode bootstrap: %w", err)
		}
		newCursor := time.Now().UTC().Format(time.RFC3339)
		if len(body) > 0 && body[0].LastUpdated != "" {
			newCursor = body[0].LastUpdated
		}

		return nil, newCursor, nil
	}
	q := url.Values{}
	q.Set("filter", fmt.Sprintf(`lastUpdated gt "%s"`, cursor))
	q.Set("limit", fmt.Sprintf("%d", defaultPageSize))
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v1/"+ns.ID+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: okta: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("okta: delta status=%d", resp.StatusCode)
	}
	var body []struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		LastUpdated string `json:"lastUpdated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("okta: decode delta: %w", err)
	}
	newCursor := cursor
	changes := make([]connector.DocumentChange, 0, len(body))
	for _, p := range body {
		updated, _ := time.Parse(time.RFC3339, p.LastUpdated)
		kind := connector.ChangeUpserted
		if strings.EqualFold(p.Status, "DEPROVISIONED") {
			kind = connector.ChangeDeleted
		}
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          p.ID,
				ETag:        p.LastUpdated,
				UpdatedAt:   updated,
			},
		})
		if p.LastUpdated > newCursor {
			newCursor = p.LastUpdated
		}
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper for relative paths.
func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	return o.doAbsolute(ctx, conn, method, strings.TrimRight(conn.baseURL, "/")+path, body)
}

// doAbsolute issues a request against an absolute URL — used for
// the Link: rel=next header that Okta returns as a fully-qualified
// URL.
func (o *Connector) doAbsolute(ctx context.Context, conn *connection, method, target string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("okta: build request: %w", err)
	}
	req.Header.Set("Authorization", "SSWS "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("okta: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Okta connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
