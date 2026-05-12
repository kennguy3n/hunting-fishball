// Package googleworkspace implements the Google Workspace
// Directory SourceConnector + DeltaSyncer against the Admin SDK
// Directory API at https://admin.googleapis.com/admin/directory/v1.
// Authentication uses an OAuth 2.0 bearer access token. We use
// stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "ya29....", // required
//	  "customer":     "my_customer" // optional, defaults to "my_customer"
//	}
//
// The connector exposes two synthetic namespaces — "users" and
// "groups" — and uses the Admin SDK's `updatedMin` (RFC3339)
// query parameter for incremental synchronisation. Suspended
// users surface as `connector.ChangeDeleted`.
package googleworkspace

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
	Name = "google_workspace"

	defaultBaseURL  = "https://admin.googleapis.com/admin/directory/v1"
	defaultCustomer = "my_customer"
	defaultPageSize = 200
	// maxDeltaPages caps the inner page-walk in DeltaSync so a
	// runaway nextPageToken loop (or a backfill that legitimately
	// exceeds 200*N changes in one window) cannot pin a worker
	// indefinitely. 50 pages * 200 results = 10,000 changes per
	// sync, which is well above any realistic per-window delta.
	// Exceeding the cap surfaces an error rather than silently
	// truncating results.
	maxDeltaPages = 50
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	Customer    string `json:"customer,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the Admin SDK base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	customer string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (g *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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

// Connect calls /users?maxResults=1 against the configured
// customer as a cheap auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	customer := creds.Customer
	if customer == "" {
		customer = defaultCustomer
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken, customer: customer}
	resp, err := g.do(ctx, conn, http.MethodGet, "/users?customer="+url.QueryEscape(customer)+"&maxResults=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: google_workspace: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google_workspace: /users status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns two synthetic namespaces: users + groups.
func (g *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{
		{ID: "users", Name: "Users", Kind: "users"},
		{ID: "groups", Name: "Groups", Kind: "groups"},
	}, nil
}

type principalIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	token   string
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
	if !it.first {
		it.first = true
	} else if it.token == "" {
		it.done = true

		return false
	}
	q := url.Values{}
	q.Set("customer", it.conn.customer)
	q.Set("maxResults", fmt.Sprintf("%d", defaultPageSize))
	if it.token != "" {
		q.Set("pageToken", it.token)
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/"+it.ns.ID+"?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: google_workspace: list %s status=%d", connector.ErrRateLimited, it.ns.ID, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("google_workspace: list %s status=%d", it.ns.ID, resp.StatusCode)

		return false
	}
	var body struct {
		Users []struct {
			ID            string `json:"id"`
			PrimaryEmail  string `json:"primaryEmail"`
			Suspended     bool   `json:"suspended"`
			LastLoginTime string `json:"lastLoginTime"`
			CreationTime  string `json:"creationTime"`
		} `json:"users"`
		Groups []struct {
			ID          string `json:"id"`
			Email       string `json:"email"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"groups"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("google_workspace: decode %s: %w", it.ns.ID, err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	if it.ns.ID == "users" {
		for _, u := range body.Users {
			it.page = append(it.page, connector.DocumentRef{
				NamespaceID: it.ns.ID,
				ID:          u.ID,
			})
		}
	} else {
		for _, gr := range body.Groups {
			it.page = append(it.page, connector.DocumentRef{
				NamespaceID: it.ns.ID,
				ID:          gr.ID,
			})
		}
	}
	it.token = body.NextPageToken
	if it.token == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}

	return true
}

// ListDocuments enumerates users / groups in the namespace.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("google_workspace: bad connection type")
	}

	return &principalIterator{g: g, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single user or group by ID.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("google_workspace: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/"+ref.NamespaceID+"/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: google_workspace: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google_workspace: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("google_workspace: read body: %w", err)
	}
	var body struct {
		ID           string          `json:"id"`
		PrimaryEmail string          `json:"primaryEmail"`
		Email        string          `json:"email"`
		Name         json.RawMessage `json:"name"`
		CreationTime string          `json:"creationTime"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.PrimaryEmail
	if title == "" {
		title = body.Email
	}
	if title == "" && len(body.Name) > 0 {
		// Users carry `name` as a {fullName: "..."} object; groups
		// carry it as a flat string. Try both shapes.
		var userName struct {
			FullName string `json:"fullName"`
		}
		if err := json.Unmarshal(body.Name, &userName); err == nil && userName.FullName != "" {
			title = userName.FullName
		} else {
			var groupName string
			if err := json.Unmarshal(body.Name, &groupName); err == nil {
				title = groupName
			}
		}
	}
	if title == "" {
		title = body.ID
	}
	created, _ := time.Parse(time.RFC3339, body.CreationTime)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.PrimaryEmail,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: created,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Admin SDK push notifications use
// the watch endpoint which is out of scope for stdlib ingestion.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync filters by `updatedMin=<RFC3339>`. Empty cursor on
// the first call returns the newest principal's update timestamp
// (DESC + max-results=1) without backfilling history.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("google_workspace: bad connection type")
	}
	if cursor == "" {
		q := url.Values{}
		q.Set("customer", conn.customer)
		q.Set("orderBy", "email")
		q.Set("sortOrder", "DESCENDING")
		q.Set("maxResults", "1")
		resp, err := g.do(ctx, conn, http.MethodGet, "/"+ns.ID+"?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: google_workspace: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("google_workspace: bootstrap status=%d", resp.StatusCode)
		}

		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	// Walk through every page tied to this updatedMin window.
	// Admin SDK pagination is opaque (nextPageToken), so we
	// aggregate inside a single DeltaSync call rather than
	// encoding the token in the public cursor; the cursor stays
	// a clean RFC3339 timestamp, matching the other change-since
	// connectors (BambooHR / Workday / Personio).
	var changes []connector.DocumentChange
	pageToken := ""
	for page := 0; page < maxDeltaPages; page++ {
		q := url.Values{}
		q.Set("customer", conn.customer)
		q.Set("maxResults", fmt.Sprintf("%d", defaultPageSize))
		if ns.ID == "users" {
			// Admin SDK users.list accepts `query=email:* ...`
			// for arbitrary filters; updatedMin is the canonical
			// delta signal documented for the directory API.
			q.Set("query", "email:* ")
		}
		q.Set("updatedMin", cursor)
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		resp, err := g.do(ctx, conn, http.MethodGet, "/"+ns.ID+"?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("%w: google_workspace: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, cursor, fmt.Errorf("google_workspace: delta status=%d", resp.StatusCode)
		}
		var body struct {
			Users []struct {
				ID           string `json:"id"`
				PrimaryEmail string `json:"primaryEmail"`
				Suspended    bool   `json:"suspended"`
				CreationTime string `json:"creationTime"`
			} `json:"users"`
			Groups []struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"groups"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("google_workspace: decode delta: %w", err)
		}
		_ = resp.Body.Close()
		if ns.ID == "users" {
			for _, u := range body.Users {
				kind := connector.ChangeUpserted
				if u.Suspended {
					kind = connector.ChangeDeleted
				}
				changes = append(changes, connector.DocumentChange{
					Kind: kind,
					Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: u.ID},
				})
			}
		} else {
			for _, gr := range body.Groups {
				changes = append(changes, connector.DocumentChange{
					Kind: connector.ChangeUpserted,
					Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: gr.ID},
				})
			}
		}
		if body.NextPageToken == "" {
			return changes, time.Now().UTC().Format(time.RFC3339), nil
		}
		pageToken = body.NextPageToken
	}

	return changes, cursor, fmt.Errorf("google_workspace: delta exceeded max pages (%d) for updatedMin=%s; reduce sync window", maxDeltaPages, cursor)
}

// do is the shared HTTP helper.
func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("google_workspace: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("google_workspace: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Google Workspace connector with the
// global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
