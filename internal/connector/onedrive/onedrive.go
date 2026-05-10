// Package onedrive implements the OneDrive SourceConnector against
// the Microsoft Graph API. It mirrors the SharePoint connector but is
// scoped to personal drives (`/me/drive`) instead of `/sites`.
//
// Credentials must be a JSON blob with `access_token`. The control
// plane decrypts internal/credential storage before supplying bytes
// to Connect.
package onedrive

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
	Name           = "onedrive"
	defaultBaseURL = "https://graph.microsoft.com/v1.0"
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

// WithHTTPClient overrides the http.Client used for upstream calls.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the Graph API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector with the supplied options.
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

// Validate checks that cfg is well-formed for OneDrive.
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

// Connect performs an auth check against `/me`.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, accessToken: creds.AccessToken}
	resp, err := g.do(ctx, conn, http.MethodGet, "/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("onedrive: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces returns a single synthetic namespace pointing at
// `/me/drive` — OneDrive only exposes one drive per principal.
func (g *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "me", Name: "OneDrive", Kind: "personal_drive"}}, nil
}

type docIterator struct {
	g       *Connector
	conn    *connection
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
	path := "/me/drive/root/children"
	if it.cursor != "" {
		path = it.cursor
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("onedrive: list status=%d", resp.StatusCode)
		return false
	}
	var body struct {
		Value []struct {
			ID                   string `json:"id"`
			Name                 string `json:"name"`
			ETag                 string `json:"eTag"`
			LastModifiedDateTime string `json:"lastModifiedDateTime"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("onedrive: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, f := range body.Value {
		modified, _ := time.Parse(time.RFC3339, f.LastModifiedDateTime)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: "me", ID: f.ID, ETag: f.ETag, UpdatedAt: modified,
		})
	}
	if body.NextLink == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = strings.TrimPrefix(body.NextLink, it.g.baseURL)
	}
	return true
}

// ListDocuments returns a paginated iterator.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, _ connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("onedrive: bad connection type")
	}
	return &docIterator{g: g, conn: conn}, nil
}

// FetchDocument returns the file's metadata + content stream.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("onedrive: bad connection type")
	}
	metaResp, err := g.do(ctx, conn, http.MethodGet, "/me/drive/items/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = metaResp.Body.Close() }()
	if metaResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("onedrive: get item status=%d", metaResp.StatusCode)
	}
	var meta struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		File struct {
			MimeType string `json:"mimeType"`
		} `json:"file"`
		LastModifiedDateTime string `json:"lastModifiedDateTime"`
		CreatedDateTime      string `json:"createdDateTime"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("onedrive: decode meta: %w", err)
	}
	bodyResp, err := g.do(ctx, conn, http.MethodGet, "/me/drive/items/"+url.PathEscape(ref.ID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	if bodyResp.StatusCode != http.StatusOK {
		_ = bodyResp.Body.Close()
		return nil, fmt.Errorf("onedrive: download status=%d", bodyResp.StatusCode)
	}
	created, _ := time.Parse(time.RFC3339, meta.CreatedDateTime)
	modified, _ := time.Parse(time.RFC3339, meta.LastModifiedDateTime)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  meta.File.MimeType,
		Title:     meta.Name,
		Size:      meta.Size,
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   bodyResp.Body,
		Metadata:  map[string]string{"item_id": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported (clients use DeltaSync).
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync implements connector.DeltaSyncer using Graph's
// `/me/drive/root/delta` endpoint.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, _ connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("onedrive: bad connection type")
	}
	path := "/me/drive/root/delta"
	if cursor != "" {
		path = strings.TrimPrefix(cursor, g.baseURL)
	}
	resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("onedrive: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID                   string    `json:"id"`
			Deleted              *struct{} `json:"deleted"`
			LastModifiedDateTime string    `json:"lastModifiedDateTime"`
		} `json:"value"`
		DeltaLink string `json:"@odata.deltaLink"`
		NextLink  string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("onedrive: decode delta: %w", err)
	}
	changes := make([]connector.DocumentChange, 0, len(body.Value))
	for _, item := range body.Value {
		kind := connector.ChangeUpserted
		if item.Deleted != nil {
			kind = connector.ChangeDeleted
		}
		modified, _ := time.Parse(time.RFC3339, item.LastModifiedDateTime)
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: "me", ID: item.ID, UpdatedAt: modified},
		})
	}
	next := body.DeltaLink
	if next == "" {
		next = body.NextLink
	}
	if next == "" {
		next = cursor
	}
	return changes, next, nil
}

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("onedrive: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrive: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the OneDrive connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
