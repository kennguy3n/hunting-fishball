// Package box implements the Box.com SourceConnector against Box
// API v2.
//
// We list children under a configurable root folder (default "0" =
// the user's root) using `/folders/{id}/items` and use Box's
// `/events` stream for the optional DeltaSyncer capability.
//
// Credentials must be a JSON blob with `access_token`. The control
// plane decrypts internal/credential storage before supplying bytes
// to Connect.
package box

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
	Name           = "box"
	defaultBaseURL = "https://api.box.com/2.0"
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

// Validate checks cfg for the Box connector.
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
		return nil, fmt.Errorf("box: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces returns a synthetic root folder.
func (g *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "0", Name: "All Files", Kind: "folder"}}, nil
}

type docIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	page    []connector.DocumentRef
	pageIdx int
	offset  int
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
	q.Set("limit", "100")
	q.Set("offset", fmt.Sprintf("%d", it.offset))
	q.Set("fields", "id,name,etag,modified_at,size,type")
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/folders/"+url.PathEscape(it.ns.ID)+"/items?"+q.Encode(), nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: box: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("box: list status=%d", resp.StatusCode)
		return false
	}
	var body struct {
		TotalCount int `json:"total_count"`
		Entries    []struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Name       string `json:"name"`
			ETag       string `json:"etag"`
			ModifiedAt string `json:"modified_at"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("box: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, e := range body.Entries {
		if e.Type != "file" {
			continue
		}
		modified, _ := time.Parse(time.RFC3339, e.ModifiedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          e.ID,
			ETag:        e.ETag,
			UpdatedAt:   modified,
		})
	}
	it.offset += len(body.Entries)
	if it.offset >= body.TotalCount {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}
	return true
}

// ListDocuments enumerates the namespace's children.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("box: bad connection type")
	}
	if ns.ID == "" {
		ns.ID = "0"
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the file's metadata + a stream from
// `/files/{id}/content`. Box returns a 302 redirect to a signed
// download URL; Go's http.Client follows it automatically.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("box: bad connection type")
	}
	metaResp, err := g.do(ctx, conn, http.MethodGet, "/files/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = metaResp.Body.Close() }()
	if metaResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("box: get file status=%d", metaResp.StatusCode)
	}
	var meta struct {
		Name       string `json:"name"`
		Size       int64  `json:"size"`
		CreatedAt  string `json:"created_at"`
		ModifiedAt string `json:"modified_at"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("box: decode meta: %w", err)
	}
	bodyResp, err := g.do(ctx, conn, http.MethodGet, "/files/"+url.PathEscape(ref.ID)+"/content", nil)
	if err != nil {
		return nil, err
	}
	if bodyResp.StatusCode != http.StatusOK {
		_ = bodyResp.Body.Close()
		return nil, fmt.Errorf("box: download status=%d", bodyResp.StatusCode)
	}
	created, _ := time.Parse(time.RFC3339, meta.CreatedAt)
	modified, _ := time.Parse(time.RFC3339, meta.ModifiedAt)
	return &connector.Document{
		Ref:       ref,
		Title:     meta.Name,
		Size:      meta.Size,
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   bodyResp.Body,
		Metadata:  map[string]string{"file_id": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync queries the Box `/events` stream. The cursor is the
// `next_stream_position` returned by the previous call. An empty
// cursor returns the current head position without backfilling.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, _ connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("box: bad connection type")
	}
	q := url.Values{}
	q.Set("stream_position", cursor)
	if cursor == "" {
		q.Set("stream_position", "now")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/events?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("box: events status=%d", resp.StatusCode)
	}
	var body struct {
		Entries []struct {
			EventType string `json:"event_type"`
			Source    struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"source"`
		} `json:"entries"`
		NextStreamPosition int64 `json:"next_stream_position"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("box: decode events: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Entries))
	for _, e := range body.Entries {
		if e.Source.Type != "file" {
			continue
		}
		kind := connector.ChangeUpserted
		if strings.Contains(strings.ToLower(e.EventType), "delete") || strings.Contains(strings.ToLower(e.EventType), "trash") {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{ID: e.Source.ID},
		})
	}
	next := fmt.Sprintf("%d", body.NextStreamPosition)
	if body.NextStreamPosition == 0 && cursor != "" {
		next = cursor
	}
	return out, next, nil
}

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("box: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("box: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the Box connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
