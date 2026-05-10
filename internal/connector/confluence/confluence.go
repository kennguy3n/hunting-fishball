// Package confluence implements the Confluence Cloud SourceConnector
// against the Atlassian REST API v2.
//
// We use `/wiki/api/v2/pages` for enumeration and the same endpoint
// with a `body-format=storage` arg + `lastModified` filter for the
// optional DeltaSyncer capability. Stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "email": "u@x.com", "api_token": "...", "site_url": "https://acme.atlassian.net" }
package confluence

import (
	"bytes"
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
const Name = "confluence"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Email    string `json:"email"`
	APIToken string `json:"api_token"`
	SiteURL  string `json:"site_url"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the http.Client used for upstream calls.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL pins the base URL for tests; overrides the credentials'
// site_url.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, o := range opts {
		o(c)
	}
	return c
}

type connection struct {
	tenantID, sourceID string
	authHeader         string
	siteURL            string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the Confluence connector.
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

// Connect opens a connection by performing a /wiki/api/v2/spaces auth
// check.
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
	resp, err := g.do(ctx, conn, http.MethodGet, "/wiki/api/v2/spaces?limit=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces enumerates Confluence spaces.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/wiki/api/v2/spaces?limit=100", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence: list spaces status=%d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ID   string `json:"id"`
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("confluence: decode spaces: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body.Results))
	for _, s := range body.Results {
		out = append(out, connector.Namespace{
			ID:       s.ID,
			Name:     s.Name,
			Kind:     "space",
			Metadata: map[string]string{"key": s.Key},
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
	cursor  string
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
	q.Set("space-id", it.ns.ID)
	q.Set("limit", "100")
	if it.cursor != "" {
		q.Set("cursor", it.cursor)
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/wiki/api/v2/pages?"+q.Encode(), nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("confluence: list pages status=%d", resp.StatusCode)
		return false
	}
	var body struct {
		Results []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Version struct {
				Number    int    `json:"number"`
				CreatedAt string `json:"createdAt"`
			} `json:"version"`
		} `json:"results"`
		Links struct {
			Next string `json:"next"`
		} `json:"_links"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("confluence: decode pages: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, r := range body.Results {
		modified, _ := time.Parse(time.RFC3339, r.Version.CreatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          r.ID,
			ETag:        fmt.Sprintf("v%d", r.Version.Number),
			UpdatedAt:   modified,
		})
	}
	if body.Links.Next == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		// Confluence returns the cursor as a query string suffix.
		if u, err := url.Parse(body.Links.Next); err == nil {
			it.cursor = u.Query().Get("cursor")
		}
	}
	return true
}

// ListDocuments enumerates pages within a space.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence: bad connection type")
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the page with body-format=storage.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/wiki/api/v2/pages/"+url.PathEscape(ref.ID)+"?body-format=storage", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("confluence: get page status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		Title   string `json:"title"`
		Version struct {
			CreatedAt string `json:"createdAt"`
		} `json:"version"`
		CreatedAt string `json:"createdAt"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse(time.RFC3339, meta.CreatedAt)
	modified, _ := time.Parse(time.RFC3339, meta.Version.CreatedAt)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/atlassian-confluence-storage+xml",
		Title:     meta.Title,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(bytes.NewReader(raw)),
		Metadata:  map[string]string{"page_id": ref.ID, "space_id": ref.NamespaceID},
	}, nil
}

// Subscribe returns ErrNotSupported.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync re-runs the page listing and returns pages whose
// version.createdAt is after the cursor timestamp.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("confluence: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cutoff, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("confluence: invalid cursor: %w", err)
	}
	q := url.Values{}
	q.Set("space-id", ns.ID)
	q.Set("limit", "100")
	resp, err := g.do(ctx, conn, http.MethodGet, "/wiki/api/v2/pages?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("confluence: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Version struct {
				CreatedAt string `json:"createdAt"`
			} `json:"version"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("confluence: decode delta: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Results))
	high := cutoff
	for _, r := range body.Results {
		modified, _ := time.Parse(time.RFC3339, r.Version.CreatedAt)
		if !modified.After(cutoff) {
			continue
		}
		if modified.After(high) {
			high = modified
		}
		kind := connector.ChangeUpserted
		if strings.EqualFold(r.Status, "trashed") || strings.EqualFold(r.Status, "deleted") {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: r.ID, UpdatedAt: modified},
		})
	}
	return out, high.Format(time.RFC3339), nil
}

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.siteURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("confluence: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHeader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the Confluence connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
