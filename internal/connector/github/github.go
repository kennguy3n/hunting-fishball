// Package github implements the GitHub SourceConnector against the
// REST API v3.
//
// We list repositories under a configurable owner (or the
// authenticated user) and enumerate issues per repo. The optional
// DeltaSyncer capability uses GitHub's `since` query parameter on
// `/issues`. The optional WebhookReceiver decodes push/issues events.
//
// Credentials must be a JSON blob with a personal access token:
//
//	{ "access_token": "ghp_..." }
package github

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
	Name           = "github"
	defaultBaseURL = "https://api.github.com"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	Owner       string `json:"owner,omitempty"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	webhookSecret []byte
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// WithWebhookSecret enables signature verification on incoming
// webhooks. GitHub signs the body with the configured secret and
// sends the digest in `X-Hub-Signature-256` (Phase 8 / Task 9).
func WithWebhookSecret(secret string) Option {
	return func(g *Connector) { g.webhookSecret = []byte(secret) }
}

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// VerifyWebhookRequest validates the GitHub `X-Hub-Signature-256`
// header against the configured webhook secret. Implements
// connector.WebhookVerifier. When no secret is configured the call
// returns nil (verification disabled).
func (g *Connector) VerifyWebhookRequest(headers map[string][]string, payload []byte) error {
	if len(g.webhookSecret) == 0 {
		return nil
	}
	return connector.VerifyHMACSHA256(g.webhookSecret, payload, connector.FirstHeader(headers, "X-Hub-Signature-256"))
}

type connection struct {
	tenantID, sourceID, accessToken, owner string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the GitHub connector.
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
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		accessToken: c.AccessToken,
		owner:       c.Owner,
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/user", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: auth status=%d", resp.StatusCode)
	}
	if conn.owner == "" {
		var u struct {
			Login string `json:"login"`
		}
		// Re-decode from a buffered copy isn't strictly needed; use a
		// fresh call so the body close above doesn't matter.
		ownerResp, ferr := g.do(ctx, conn, http.MethodGet, "/user", nil)
		if ferr == nil {
			defer func() { _ = ownerResp.Body.Close() }()
			_ = json.NewDecoder(ownerResp.Body).Decode(&u)
			conn.owner = u.Login
		}
	}
	return conn, nil
}

// ListNamespaces enumerates repos under the connection's owner.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("github: bad connection type")
	}
	path := "/user/repos?per_page=100&affiliation=owner"
	resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: list repos status=%d", resp.StatusCode)
	}
	var body []struct {
		FullName string `json:"full_name"`
		Name     string `json:"name"`
		Private  bool   `json:"private"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("github: decode repos: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body))
	for _, r := range body {
		out = append(out, connector.Namespace{
			ID:       r.FullName,
			Name:     r.Name,
			Kind:     "repo",
			Metadata: map[string]string{"private": fmt.Sprintf("%v", r.Private)},
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
	q.Set("state", "all")
	q.Set("per_page", "100")
	q.Set("page", fmt.Sprintf("%d", it.pageNum))
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/repos/"+it.ns.ID+"/issues?"+q.Encode(), nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("github: list issues status=%d", resp.StatusCode)
		return false
	}
	var body []struct {
		Number    int    `json:"number"`
		UpdatedAt string `json:"updated_at"`
		PullReq   *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("github: decode issues: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, iss := range body {
		modified, _ := time.Parse(time.RFC3339, iss.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          fmt.Sprintf("%d", iss.Number),
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

// ListDocuments enumerates issues + PRs in a repo.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("github: bad connection type")
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the issue body.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("github: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/repos/"+ref.NamespaceID+"/issues/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("github: get issue status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		Title string `json:"title"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		Body      string `json:"body"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse(time.RFC3339, meta.CreatedAt)
	modified, _ := time.Parse(time.RFC3339, meta.UpdatedAt)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     meta.Title,
		Author:    meta.User.Login,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"repo": ref.NamespaceID, "issue": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /repos/{owner}/{repo}/issues with the `since`
// parameter. The cursor is the RFC3339 timestamp of the previous
// high-water mark.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("github: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cutoff, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("github: invalid cursor: %w", err)
	}
	q := url.Values{}
	q.Set("since", cutoff.UTC().Format(time.RFC3339))
	q.Set("state", "all")
	q.Set("per_page", "100")
	resp, err := g.do(ctx, conn, http.MethodGet, "/repos/"+ns.ID+"/issues?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("github: delta status=%d", resp.StatusCode)
	}
	var body []struct {
		Number    int    `json:"number"`
		State     string `json:"state"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("github: decode delta: %w", err)
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
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: fmt.Sprintf("%d", iss.Number), UpdatedAt: modified},
		})
	}
	return out, high.UTC().Format(time.RFC3339), nil
}

// HandleWebhook decodes a GitHub webhook payload (issues / pull
// request / push events). Signature verification is left to the
// caller — the platform decodes the X-Hub-Signature header before
// invoking this method.
func (g *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	var body struct {
		Action string `json:"action"`
		Issue  *struct {
			Number int `json:"number"`
		} `json:"issue"`
		PullRequest *struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("github: webhook decode: %w", err)
	}
	var id string
	switch {
	case body.Issue != nil:
		id = fmt.Sprintf("%d", body.Issue.Number)
	case body.PullRequest != nil:
		id = fmt.Sprintf("%d", body.PullRequest.Number)
	}
	if id == "" {
		return nil, nil
	}
	kind := connector.ChangeUpserted
	if strings.EqualFold(body.Action, "deleted") {
		kind = connector.ChangeDeleted
	}
	return []connector.DocumentChange{{
		Kind: kind,
		Ref:  connector.DocumentRef{NamespaceID: body.Repository.FullName, ID: id},
	}}, nil
}

// WebhookPath is the path suffix the platform mounts the receiver at.
func (g *Connector) WebhookPath() string { return "/github" }

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the GitHub connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
