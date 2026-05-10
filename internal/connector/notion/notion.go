// Package notion implements the Notion SourceConnector against the
// Notion API (api.notion.com/v1).
//
// We use `/search` for document enumeration and the
// `last_edited_time` filter for the optional DeltaSyncer capability.
// Stdlib net/http only.
//
// Credentials must be a JSON blob with `access_token`. The control
// plane decrypts internal/credential storage before supplying bytes
// to Connect.
package notion

import (
	"bytes"
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
	Name           = "notion"
	defaultBaseURL = "https://api.notion.com/v1"

	// notionVersion pins the API version. Notion strongly recommends
	// pinning so server-side changes don't break clients silently.
	notionVersion = "2022-06-28"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
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

// WithBaseURL overrides the API base URL — used by tests.
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
	tenantID, sourceID, accessToken string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the Notion connector.
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

// Connect performs an auth check via `/users/me`.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, accessToken: c.AccessToken}
	resp, err := g.do(ctx, conn, http.MethodGet, "/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("notion: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces returns a single workspace namespace. Notion's
// /search runs against all pages the integration has access to, so
// the namespace is mostly cosmetic.
func (g *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "workspace", Name: "Workspace", Kind: "workspace"}}, nil
}

type docIterator struct {
	g       *Connector
	conn    *connection
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
	err     error
	done    bool
	since   time.Time
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
	body := map[string]any{
		"filter":    map[string]string{"property": "object", "value": "page"},
		"page_size": 100,
		"sort":      map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
	}
	if it.cursor != "" {
		body["start_cursor"] = it.cursor
	}
	resp, err := it.g.post(ctx, it.conn, "/search", body)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("notion: search status=%d", resp.StatusCode)
		return false
	}
	var decoded struct {
		Results []struct {
			ID             string `json:"id"`
			LastEditedTime string `json:"last_edited_time"`
		} `json:"results"`
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		it.err = fmt.Errorf("notion: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, r := range decoded.Results {
		modified, _ := time.Parse(time.RFC3339, r.LastEditedTime)
		if !it.since.IsZero() && modified.Before(it.since) {
			continue
		}
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: "workspace",
			ID:          r.ID,
			UpdatedAt:   modified,
		})
	}
	it.cursor = decoded.NextCursor
	if !decoded.HasMore {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}
	return true
}

// ListDocuments enumerates pages via Notion `/search`.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, _ connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("notion: bad connection type")
	}
	return &docIterator{g: g, conn: conn, since: opts.Since}, nil
}

// FetchDocument returns the page metadata. We do NOT walk the
// content blocks here — Stage 2 of the pipeline (parse) is responsible
// for that. This call returns the page meta + a JSON content blob so
// the parser can choose how to deserialise.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("notion: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/pages/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("notion: get page status=%d", resp.StatusCode)
	}
	// Drain into a buffer so we can both reflect metadata fields and
	// expose the raw JSON to downstream stages.
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		ID             string `json:"id"`
		CreatedTime    string `json:"created_time"`
		LastEditedTime string `json:"last_edited_time"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse(time.RFC3339, meta.CreatedTime)
	modified, _ := time.Parse(time.RFC3339, meta.LastEditedTime)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		CreatedAt: created,
		UpdatedAt: modified,
		Size:      int64(len(raw)),
		Content:   io.NopCloser(bytes.NewReader(raw)),
		Metadata:  map[string]string{"page_id": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /search with a `last_edited_time` filter. The
// cursor is the RFC3339 timestamp of the previous high-water mark;
// the response contains items with last_edited_time > cursor.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, _ connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("notion: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cutoff, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("notion: invalid cursor: %w", err)
	}
	body := map[string]any{
		"filter":    map[string]string{"property": "object", "value": "page"},
		"page_size": 100,
		"sort":      map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
	}
	resp, err := g.post(ctx, conn, "/search", body)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("notion: search status=%d", resp.StatusCode)
	}
	var decoded struct {
		Results []struct {
			ID             string `json:"id"`
			Archived       bool   `json:"archived"`
			LastEditedTime string `json:"last_edited_time"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, "", fmt.Errorf("notion: decode: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(decoded.Results))
	high := cutoff
	for _, r := range decoded.Results {
		modified, _ := time.Parse(time.RFC3339, r.LastEditedTime)
		if !modified.After(cutoff) {
			continue
		}
		if modified.After(high) {
			high = modified
		}
		kind := connector.ChangeUpserted
		if r.Archived {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: "workspace", ID: r.ID, UpdatedAt: modified},
		})
	}
	return out, high.Format(time.RFC3339), nil
}

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("notion: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notion: %s %s: %w", method, target, err)
	}
	return resp, nil
}

func (g *Connector) post(ctx context.Context, conn *connection, path string, body any) (*http.Response, error) {
	buf := &bytes.Buffer{}
	if body != nil {
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
	}
	return g.do(ctx, conn, http.MethodPost, path, buf)
}

// Register hooks the Notion connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
