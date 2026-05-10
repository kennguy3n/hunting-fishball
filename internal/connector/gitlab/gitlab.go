// Package gitlab implements the GitLab SourceConnector against the
// GitLab REST API v4.
//
// We list projects via `/projects?membership=true` and enumerate
// issues per project. The optional DeltaSyncer capability uses the
// `updated_after` query parameter; the optional WebhookReceiver
// decodes issue / merge request events.
//
// Credentials must be a JSON blob with a personal access token:
//
//	{ "access_token": "glpat-...", "site_url": "https://gitlab.com" }
package gitlab

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
	Name           = "gitlab"
	defaultBaseURL = "https://gitlab.com/api/v4"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	SiteURL     string `json:"site_url,omitempty"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the API base URL — used by tests; overrides
// credentials' site_url.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

type connection struct {
	tenantID, sourceID, accessToken, baseURL string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the GitLab connector.
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
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if c.AccessToken == "" {
		return fmt.Errorf("%w: access_token required", connector.ErrInvalidConfig)
	}
	return nil
}

// Connect performs a `/user` auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	site := strings.TrimRight(g.baseURL, "/")
	if c.SiteURL != "" && site == defaultBaseURL {
		site = strings.TrimRight(c.SiteURL, "/") + "/api/v4"
	}
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		accessToken: c.AccessToken,
		baseURL:     site,
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/user", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitlab: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces enumerates projects the user is a member of.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gitlab: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/projects?membership=true&per_page=100", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitlab: list projects status=%d", resp.StatusCode)
	}
	var body []struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		Name              string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("gitlab: decode projects: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body))
	for _, p := range body {
		out = append(out, connector.Namespace{
			ID:       fmt.Sprintf("%d", p.ID),
			Name:     p.Name,
			Kind:     "project",
			Metadata: map[string]string{"path": p.PathWithNamespace},
		})
	}
	return out, nil
}

type docIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	page    []connector.DocumentRef
	pageIdx int
	pageNum int
	err     error
	done    bool
}

func (it *docIterator) Next(ctx context.Context) bool {
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

func (it *docIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}
	return it.page[it.pageIdx-1]
}

func (it *docIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}
	return it.err
}

func (it *docIterator) Close() error { return nil }

func (it *docIterator) fetchPage(ctx context.Context) bool {
	it.pageNum++
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("page", fmt.Sprintf("%d", it.pageNum))
	q.Set("scope", "all")
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/projects/"+it.ns.ID+"/issues?"+q.Encode(), nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("gitlab: list issues status=%d", resp.StatusCode)
		return false
	}
	var body []struct {
		IID       int    `json:"iid"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("gitlab: decode issues: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, iss := range body {
		modified, _ := time.Parse(time.RFC3339, iss.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          fmt.Sprintf("%d", iss.IID),
			UpdatedAt:   modified,
		})
	}
	if len(body) < 100 {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}
	return true
}

// ListDocuments enumerates issues in a project.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gitlab: bad connection type")
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the issue body.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gitlab: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/projects/"+ref.NamespaceID+"/issues/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("gitlab: get issue status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		Title  string `json:"title"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse(time.RFC3339, meta.CreatedAt)
	modified, _ := time.Parse(time.RFC3339, meta.UpdatedAt)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     meta.Title,
		Author:    meta.Author.Username,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"project_id": ref.NamespaceID, "issue_iid": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /projects/{id}/issues with `updated_after`. The
// cursor is the RFC3339 timestamp.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("gitlab: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cutoff, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("gitlab: invalid cursor: %w", err)
	}
	q := url.Values{}
	q.Set("updated_after", cutoff.UTC().Format(time.RFC3339))
	q.Set("per_page", "100")
	q.Set("scope", "all")
	resp, err := g.do(ctx, conn, http.MethodGet, "/projects/"+ns.ID+"/issues?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("gitlab: delta status=%d", resp.StatusCode)
	}
	var body []struct {
		IID       int    `json:"iid"`
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("gitlab: decode delta: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body))
	high := cutoff
	for _, iss := range body {
		modified, _ := time.Parse(time.RFC3339, iss.UpdatedAt)
		if !modified.After(cutoff) {
			continue
		}
		if modified.After(high) {
			high = modified
		}
		out = append(out, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: fmt.Sprintf("%d", iss.IID), UpdatedAt: modified},
		})
	}
	return out, high.UTC().Format(time.RFC3339), nil
}

// HandleWebhook decodes a GitLab webhook payload.
func (g *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	var body struct {
		ObjectKind       string `json:"object_kind"`
		ObjectAttributes *struct {
			IID    int    `json:"iid"`
			Action string `json:"action"`
		} `json:"object_attributes"`
		Project struct {
			ID int `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("gitlab: webhook decode: %w", err)
	}
	if body.ObjectAttributes == nil || body.ObjectAttributes.IID == 0 {
		return nil, nil
	}
	kind := connector.ChangeUpserted
	if strings.EqualFold(body.ObjectAttributes.Action, "close") || strings.EqualFold(body.ObjectAttributes.Action, "destroyed") {
		// Closing isn't deletion in GitLab; treat as upsert with the
		// updated state. Only "destroyed" is a delete.
		if strings.EqualFold(body.ObjectAttributes.Action, "destroyed") {
			kind = connector.ChangeDeleted
		}
	}
	return []connector.DocumentChange{{
		Kind: kind,
		Ref: connector.DocumentRef{
			NamespaceID: fmt.Sprintf("%d", body.Project.ID),
			ID:          fmt.Sprintf("%d", body.ObjectAttributes.IID),
		},
	}}, nil
}

// WebhookPath is the path suffix the platform mounts the receiver at.
func (g *Connector) WebhookPath() string { return "/gitlab" }

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	if conn.baseURL == "" {
		target = strings.TrimRight(g.baseURL, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("gitlab: build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the GitLab connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
