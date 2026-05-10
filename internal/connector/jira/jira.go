// Package jira implements the Jira Cloud SourceConnector against
// the Atlassian REST API v3.
//
// We use `/rest/api/3/search` with JQL `project = X` for enumeration
// and the same endpoint with `updated >= ?` for the optional
// DeltaSyncer capability. Webhooks are decoded via the optional
// WebhookReceiver capability — the platform mounts the webhook path
// under /v1/connectors/jira/webhook and forwards the raw payload
// here.
//
// Credentials must be a JSON blob:
//
//	{ "email": "u@x.com", "api_token": "...", "site_url": "https://acme.atlassian.net" }
package jira

import (
	"context"
	"encoding/base64"
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
const Name = "jira"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
	SiteURL  string `json:"site_url"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	webhookSecret []byte
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the http.Client used for upstream calls.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL pins the base URL for tests; overrides credentials'
// site_url.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// WithWebhookSecret enables signature verification on incoming
// webhooks. Atlassian Jira signs the body with the configured secret
// and sends the SHA-256 digest in `X-Hub-Signature-256` (Phase 8 /
// Task 9).
func WithWebhookSecret(secret string) Option {
	return func(g *Connector) { g.webhookSecret = []byte(secret) }
}

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// VerifyWebhookRequest validates the Jira `X-Hub-Signature-256`
// header against the configured webhook secret. Implements
// connector.WebhookVerifier.
func (g *Connector) VerifyWebhookRequest(headers map[string][]string, payload []byte) error {
	if len(g.webhookSecret) == 0 {
		return nil
	}
	return connector.VerifyHMACSHA256(g.webhookSecret, payload, connector.FirstHeader(headers, "X-Hub-Signature-256"))
}

type connection struct {
	tenantID, sourceID  string
	authHeader, siteURL string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the Jira connector.
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
	if c.Email == "" || c.APIToken == "" {
		return fmt.Errorf("%w: email and api_token required", connector.ErrInvalidConfig)
	}
	if c.SiteURL == "" && g.baseURL == "" {
		return fmt.Errorf("%w: site_url required", connector.ErrInvalidConfig)
	}
	return nil
}

// Connect authenticates by calling /rest/api/3/myself.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	site := strings.TrimRight(g.baseURL, "/")
	if site == "" {
		site = strings.TrimRight(c.SiteURL, "/")
	}
	auth := base64.StdEncoding.EncodeToString([]byte(c.Email + ":" + c.APIToken))
	conn := &connection{
		tenantID:   cfg.TenantID,
		sourceID:   cfg.SourceID,
		authHeader: "Basic " + auth,
		siteURL:    site,
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/rest/api/3/myself", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces returns Jira projects.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("jira: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/rest/api/3/project", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: list projects status=%d", resp.StatusCode)
	}
	var body []struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("jira: decode projects: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body))
	for _, p := range body {
		out = append(out, connector.Namespace{
			ID:       p.Key,
			Name:     p.Name,
			Kind:     "project",
			Metadata: map[string]string{"project_id": p.ID},
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
	startAt int
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
	q := url.Values{}
	q.Set("jql", fmt.Sprintf("project = %s ORDER BY updated DESC", it.ns.ID))
	q.Set("startAt", fmt.Sprintf("%d", it.startAt))
	q.Set("maxResults", "100")
	q.Set("fields", "updated,summary,issuetype,status")
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/rest/api/3/search?"+q.Encode(), nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("jira: search status=%d", resp.StatusCode)
		return false
	}
	var body struct {
		StartAt    int `json:"startAt"`
		MaxResults int `json:"maxResults"`
		Total      int `json:"total"`
		Issues     []struct {
			Key    string `json:"key"`
			Fields struct {
				Updated string `json:"updated"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("jira: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, iss := range body.Issues {
		modified, _ := time.Parse("2006-01-02T15:04:05.000-0700", iss.Fields.Updated)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          iss.Key,
			UpdatedAt:   modified,
		})
	}
	it.startAt += len(body.Issues)
	if it.startAt >= body.Total {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}
	return true
}

// ListDocuments enumerates issues in the project.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("jira: bad connection type")
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the issue body. Description + comments live
// in the same /issue/{key} payload — we hand the raw JSON back to
// the parser stage rather than projecting it here.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("jira: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("jira: get issue status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Created string `json:"created"`
			Updated string `json:"updated"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse("2006-01-02T15:04:05.000-0700", meta.Fields.Created)
	modified, _ := time.Parse("2006-01-02T15:04:05.000-0700", meta.Fields.Updated)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     meta.Fields.Summary,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"issue_key": ref.ID, "project": ref.NamespaceID},
	}, nil
}

// Subscribe returns ErrNotSupported (use webhooks or DeltaSync).
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /search with `updated >= cursor`. Returns the new
// high-water mark.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("jira: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cutoff, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("jira: invalid cursor: %w", err)
	}
	jql := fmt.Sprintf("project = %s AND updated >= \"%s\" ORDER BY updated DESC",
		ns.ID, cutoff.UTC().Format("2006-01-02 15:04"))
	q := url.Values{}
	q.Set("jql", jql)
	q.Set("fields", "updated,status")
	q.Set("maxResults", "100")
	resp, err := g.do(ctx, conn, http.MethodGet, "/rest/api/3/search?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("jira: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Updated string `json:"updated"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("jira: decode delta: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Issues))
	high := cutoff
	for _, iss := range body.Issues {
		modified, _ := time.Parse("2006-01-02T15:04:05.000-0700", iss.Fields.Updated)
		if !modified.After(cutoff) {
			continue
		}
		if modified.After(high) {
			high = modified
		}
		out = append(out, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: iss.Key, UpdatedAt: modified},
		})
	}
	return out, high.UTC().Format(time.RFC3339), nil
}

// HandleWebhook decodes a Jira webhook payload. We surface the issue
// key + event type as a DocumentChange.
func (g *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	var body struct {
		WebhookEvent string `json:"webhookEvent"`
		Issue        struct {
			Key string `json:"key"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("jira: webhook decode: %w", err)
	}
	if body.Issue.Key == "" {
		return nil, nil
	}
	kind := connector.ChangeUpserted
	if strings.Contains(strings.ToLower(body.WebhookEvent), "deleted") {
		kind = connector.ChangeDeleted
	}
	return []connector.DocumentChange{{
		Kind: kind,
		Ref:  connector.DocumentRef{ID: body.Issue.Key},
	}}, nil
}

// WebhookPath is the path suffix the platform mounts the receiver at.
func (g *Connector) WebhookPath() string { return "/jira" }

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.siteURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("jira: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHeader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the Jira connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
