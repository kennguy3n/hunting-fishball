// Package dropbox implements the Dropbox SourceConnector against
// Dropbox API v2.
//
// We use the `/files/list_folder` and `/files/list_folder/continue`
// endpoints for enumeration, and `/files/list_folder/get_latest_cursor`
// + `/files/list_folder/continue` for the optional DeltaSyncer
// capability. Stdlib net/http only — no SDK dependency.
//
// Credentials must be a JSON blob with `access_token`. The control
// plane decrypts internal/credential storage before supplying bytes
// to Connect.
package dropbox

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
	Name           = "dropbox"
	defaultBaseURL = "https://api.dropboxapi.com/2"
	contentBaseURL = "https://content.dropboxapi.com/2"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient *http.Client
	baseURL    string
	contentURL string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// WithContentURL overrides the content endpoint base URL — used by
// tests so list_folder + download share a server.
func WithContentURL(u string) Option { return func(g *Connector) { g.contentURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
		contentURL: contentBaseURL,
	}
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

// Validate checks cfg for the Dropbox connector.
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

// Connect performs a `/users/get_current_account` auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, accessToken: c.AccessToken}
	resp, err := g.post(ctx, conn, g.baseURL+"/users/get_current_account", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dropbox: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces returns a single root namespace — Dropbox is flat.
func (g *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "", Name: "Root", Kind: "folder"}}, nil
}

type docIterator struct {
	g       *Connector
	conn    *connection
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
	err     error
	done    bool
	hasMore bool
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
	var (
		path string
		body any
	)
	if it.cursor == "" {
		path = it.g.baseURL + "/files/list_folder"
		body = map[string]any{"path": "", "recursive": true}
	} else {
		path = it.g.baseURL + "/files/list_folder/continue"
		body = map[string]any{"cursor": it.cursor}
	}
	resp, err := it.g.post(ctx, it.conn, path, body)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("dropbox: list status=%d", resp.StatusCode)
		return false
	}
	var decoded struct {
		Entries []struct {
			Tag            string `json:".tag"`
			ID             string `json:"id"`
			Name           string `json:"name"`
			Rev            string `json:"rev"`
			ContentHash    string `json:"content_hash"`
			ServerModified string `json:"server_modified"`
		} `json:"entries"`
		Cursor  string `json:"cursor"`
		HasMore bool   `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		it.err = fmt.Errorf("dropbox: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, e := range decoded.Entries {
		if e.Tag != "file" {
			continue
		}
		modified, _ := time.Parse(time.RFC3339, e.ServerModified)
		it.page = append(it.page, connector.DocumentRef{
			ID:        e.ID,
			ETag:      e.Rev,
			UpdatedAt: modified,
		})
	}
	it.cursor = decoded.Cursor
	it.hasMore = decoded.HasMore
	if !decoded.HasMore {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}
	return true
}

// ListDocuments returns a paginated iterator over the root.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, _ connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("dropbox: bad connection type")
	}
	return &docIterator{g: g, conn: conn}, nil
}

// FetchDocument returns the file's metadata + content stream from the
// `/files/download` endpoint on api.dropboxapi.com.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("dropbox: bad connection type")
	}
	arg, _ := json.Marshal(map[string]string{"path": ref.ID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.contentURL+"/files/download", nil)
	if err != nil {
		return nil, fmt.Errorf("dropbox: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Dropbox-API-Arg", string(arg))
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dropbox: download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("dropbox: download status=%d", resp.StatusCode)
	}
	// Dropbox returns metadata in the Dropbox-API-Result header.
	var meta struct {
		Name           string `json:"name"`
		Size           int64  `json:"size"`
		ServerModified string `json:"server_modified"`
		ClientModified string `json:"client_modified"`
	}
	if h := resp.Header.Get("Dropbox-API-Result"); h != "" {
		_ = json.Unmarshal([]byte(h), &meta)
	}
	created, _ := time.Parse(time.RFC3339, meta.ClientModified)
	modified, _ := time.Parse(time.RFC3339, meta.ServerModified)
	return &connector.Document{
		Ref:       ref,
		Title:     meta.Name,
		Size:      meta.Size,
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   resp.Body,
		Metadata:  map[string]string{"path": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported — clients use DeltaSync.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync calls `/files/list_folder/continue` once an initial
// cursor has been minted via `/files/list_folder/get_latest_cursor`.
// An empty cursor returns the current cursor without backfilling.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, _ connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("dropbox: bad connection type")
	}
	if cursor == "" {
		resp, err := g.post(ctx, conn, g.baseURL+"/files/list_folder/get_latest_cursor", map[string]any{"path": "", "recursive": true})
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("dropbox: latest_cursor status=%d", resp.StatusCode)
		}
		var body struct {
			Cursor string `json:"cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("dropbox: decode cursor: %w", err)
		}
		return nil, body.Cursor, nil
	}
	resp, err := g.post(ctx, conn, g.baseURL+"/files/list_folder/continue", map[string]any{"cursor": cursor})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("dropbox: continue status=%d", resp.StatusCode)
	}
	var body struct {
		Entries []struct {
			Tag string `json:".tag"`
			ID  string `json:"id"`
		} `json:"entries"`
		Cursor string `json:"cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("dropbox: decode delta: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Entries))
	for _, e := range body.Entries {
		kind := connector.ChangeUpserted
		if e.Tag == "deleted" {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{ID: e.ID},
		})
	}
	if body.Cursor == "" {
		body.Cursor = cursor
	}
	return out, body.Cursor, nil
}

func (g *Connector) post(ctx context.Context, conn *connection, target string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
		rdr = buf
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("dropbox: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dropbox: %s: %w", target, err)
	}
	return resp, nil
}

// Register hooks the connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }

// silence unused-import warning when callers vendor this package.
var _ = strings.TrimSpace
