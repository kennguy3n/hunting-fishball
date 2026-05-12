// Package entraid implements the Microsoft Entra ID
// SourceConnector + DeltaSyncer against the Microsoft Graph API
// at https://graph.microsoft.com/v1.0. Authentication uses an
// OAuth 2.0 client-credentials bearer access token. We use stdlib
// net/http only — no msgraph-sdk-go dependency.
//
// Credentials must be a JSON blob:
//
//	{ "access_token": "eyJ0..." }   // required
//
// The connector exposes two synthetic namespaces — "users" and
// "groups" — and uses Graph's `$deltatoken` on
// `/users/delta` + `/groups/delta` for incremental synchronisation.
// Deprovisioned / disabled users surface as
// `connector.ChangeDeleted` so the downstream index converges.
package entraid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

const (
	// Name is the registry-visible connector name.
	Name = "entra_id"

	defaultBaseURL = "https://graph.microsoft.com/v1.0"

	// maxDeltaPages caps the number of @odata.nextLink hops
	// DeltaSync walks per invocation before surfacing the
	// truncation as an error. Mirrors the safety guard used by
	// google_workspace, personio, and workday.
	maxDeltaPages = 50
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
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
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the Graph API base URL — used by tests.
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

// Connect calls /organization?$top=1 as a cheap auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken}
	resp, err := g.do(ctx, conn, http.MethodGet, "/organization?$top=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: entra_id: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("entra_id: /organization status=%d", resp.StatusCode)
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

// principalIterator paginates `/users` or `/groups` via Graph's
// `@odata.nextLink` envelope.
type principalIterator struct {
	g       *Connector
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
	var path string
	if !it.first {
		it.first = true
		path = "/" + it.ns.ID + "?$top=100"
	} else if it.nextURL != "" {
		path = it.nextURL
	} else {
		it.done = true

		return false
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: entra_id: list %s status=%d", connector.ErrRateLimited, it.ns.ID, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("entra_id: list %s status=%d", it.ns.ID, resp.StatusCode)

		return false
	}
	var body struct {
		Value []struct {
			ID                string `json:"id"`
			DisplayName       string `json:"displayName"`
			UserPrincipalName string `json:"userPrincipalName"`
			Mail              string `json:"mail"`
			AccountEnabled    *bool  `json:"accountEnabled"`
			CreatedDateTime   string `json:"createdDateTime"`
			DeletedDateTime   string `json:"deletedDateTime"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("entra_id: decode %s: %w", it.ns.ID, err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, p := range body.Value {
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          p.ID,
		})
	}
	if body.NextLink == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.nextURL = body.NextLink
	}

	return true
}

// ListDocuments enumerates users / groups in the namespace.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("entra_id: bad connection type")
	}

	return &principalIterator{g: g, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single user or group by ID.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("entra_id: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/"+ref.NamespaceID+"/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: entra_id: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("entra_id: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("entra_id: read body: %w", err)
	}
	var body struct {
		ID                string `json:"id"`
		DisplayName       string `json:"displayName"`
		UserPrincipalName string `json:"userPrincipalName"`
		Mail              string `json:"mail"`
		CreatedDateTime   string `json:"createdDateTime"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.DisplayName
	if title == "" {
		title = body.UserPrincipalName
	}
	if title == "" {
		title = body.ID
	}
	created, _ := time.Parse(time.RFC3339, body.CreatedDateTime)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.Mail,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: created,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Graph push notifications use the
// `/subscriptions` resource which is out of scope for stdlib
// ingestion. Callers fall back to DeltaSync.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op — HTTP is stateless.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses Graph's `/users/delta` + `/groups/delta`
// endpoints. The cursor is the @odata.deltaLink returned from the
// previous call. Empty cursor on the first call bootstraps via
// `/<ns>/delta?$deltatoken=latest` and returns the deltaLink
// without backfilling history.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("entra_id: bad connection type")
	}
	bootstrap := cursor == ""
	path := cursor
	if bootstrap {
		path = "/" + ns.ID + "/delta?$deltatoken=latest"
	}
	var changes []connector.DocumentChange
	for page := 0; page < maxDeltaPages; page++ {
		resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("%w: entra_id: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, cursor, fmt.Errorf("entra_id: delta status=%d", resp.StatusCode)
		}
		var body struct {
			Value []struct {
				ID                string    `json:"id"`
				AccountEnabled    *bool     `json:"accountEnabled"`
				Removed           *struct{} `json:"@removed"`
				DisplayName       string    `json:"displayName"`
				UserPrincipalName string    `json:"userPrincipalName"`
			} `json:"value"`
			NextLink  string `json:"@odata.nextLink"`
			DeltaLink string `json:"@odata.deltaLink"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("entra_id: decode delta: %w", err)
		}
		_ = resp.Body.Close()
		if !bootstrap {
			for _, p := range body.Value {
				kind := connector.ChangeUpserted
				if p.Removed != nil {
					kind = connector.ChangeDeleted
				}
				if p.AccountEnabled != nil && !*p.AccountEnabled {
					kind = connector.ChangeDeleted
				}
				changes = append(changes, connector.DocumentChange{
					Kind: kind,
					Ref: connector.DocumentRef{
						NamespaceID: ns.ID,
						ID:          p.ID,
					},
				})
			}
		}
		if body.DeltaLink != "" {
			return changes, body.DeltaLink, nil
		}
		if body.NextLink == "" {
			next := cursor
			if next == "" {
				next = path
			}

			return changes, next, nil
		}
		path = body.NextLink
	}

	return nil, "", fmt.Errorf("entra_id: delta exceeded %d pages without terminal deltaLink", maxDeltaPages)
}

// do is the shared HTTP helper. Absolute URLs are routed
// directly; relative paths are joined with baseURL. Graph's
// @odata.nextLink / @odata.deltaLink are absolute so do() must
// accept both.
func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("entra_id: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("entra_id: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Entra ID connector with the global
// registry. The init function calls this at import time.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
