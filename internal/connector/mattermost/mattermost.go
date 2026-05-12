// Package mattermost implements the Mattermost SourceConnector +
// DeltaSyncer against the Mattermost REST API v4 at
// https://<server>/api/v4. Authentication uses a personal access
// token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "abcd...",   // required (PAT)
//	  "base_url":     "https://mm.example.com" // required
//	}
package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

const (
	// Name is the registry-visible connector name.
	Name = "mattermost"

	defaultPageSize = 200
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	BaseURL     string `json:"base_url"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(m *Connector) { m.httpClient = c } }

// WithBaseURL overrides the Mattermost API base URL — used by tests.
func WithBaseURL(u string) Option { return func(m *Connector) { m.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	m := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(m)
	}

	return m
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	baseURL  string
	userID   string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (m *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	base := m.baseURL
	if base == "" {
		base = c.BaseURL
	}
	if base == "" {
		return fmt.Errorf("%w: base_url required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /users/me as the auth check.
func (m *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := m.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := m.baseURL
	if base == "" {
		base = creds.BaseURL
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken, baseURL: base}
	resp, err := m.do(ctx, conn, http.MethodGet, "/api/v4/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mattermost: /users/me status=%d", resp.StatusCode)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return nil, fmt.Errorf("mattermost: decode me: %w", err)
	}
	conn.userID = me.ID

	return conn, nil
}

// ListNamespaces returns the channels the bot/user is a member of, across teams.
func (m *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("mattermost: bad connection type")
	}
	resp, err := m.do(ctx, conn, http.MethodGet, "/api/v4/users/"+conn.userID+"/channels", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mattermost: channels list status=%d", resp.StatusCode)
	}
	var body []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Type        string `json:"type"`
		TeamID      string `json:"team_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("mattermost: decode channels: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body))
	for _, ch := range body {
		out = append(out, connector.Namespace{
			ID: ch.ID, Name: ch.DisplayName, Kind: "channel",
			Metadata: map[string]string{"team_id": ch.TeamID, "channel_type": ch.Type, "channel_name": ch.Name},
		})
	}

	return out, nil
}

// postIterator paginates posts in a channel.
type postIterator struct {
	m       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  int
	err     error
	done    bool
}

func (it *postIterator) Next(ctx context.Context) bool {
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

func (it *postIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *postIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *postIterator) Close() error { return nil }

func (it *postIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("page", strconv.Itoa(it.cursor))
	q.Set("per_page", strconv.Itoa(pageOr(it.opts.PageSize, defaultPageSize)))
	path := "/api/v4/channels/" + it.ns.ID + "/posts?" + q.Encode()
	resp, err := it.m.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("mattermost: channel posts status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Order []string `json:"order"`
		Posts map[string]struct {
			ID       string `json:"id"`
			UpdateAt int64  `json:"update_at"`
		} `json:"posts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("mattermost: decode posts: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, id := range body.Order {
		p := body.Posts[id]
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          p.ID,
			ETag:        strconv.FormatInt(p.UpdateAt, 10),
			UpdatedAt:   time.UnixMilli(p.UpdateAt),
		})
	}
	if len(body.Order) < pageOr(it.opts.PageSize, defaultPageSize) {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor++
	}

	return true
}

// ListDocuments enumerates posts in the channel namespace.
func (m *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("mattermost: bad connection type")
	}

	return &postIterator{m: m, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single post by ID.
func (m *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("mattermost: bad connection type")
	}
	resp, err := m.do(ctx, conn, http.MethodGet, "/api/v4/posts/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mattermost: get post status=%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mattermost: read post body: %w", err)
	}
	var body struct {
		ID        string `json:"id"`
		Message   string `json:"message"`
		UserID    string `json:"user_id"`
		ChannelID string `json:"channel_id"`
		CreateAt  int64  `json:"create_at"`
		UpdateAt  int64  `json:"update_at"`
	}
	_ = json.Unmarshal(raw, &body)
	title := strings.TrimSpace(body.Message)
	if title == "" {
		title = body.ID
	}
	if len(title) > 80 {
		title = title[:80]
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.UserID,
		Size:      int64(len(raw)),
		CreatedAt: time.UnixMilli(body.CreateAt),
		UpdatedAt: time.UnixMilli(body.UpdateAt),
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"channel_id": body.ChannelID, "user_id": body.UserID},
	}, nil
}

// Subscribe is unsupported — Mattermost change events are delivered
// via outgoing webhooks (HandleWebhook in a later round) or the
// websocket gateway (out of scope).
func (m *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (m *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses the channel posts endpoint with a `since` filter
// (milliseconds since epoch). The cursor is an integer ms-epoch.
// Empty cursor on the first call must return the "now" cursor
// without backfilling history — we use the most recent post's
// update_at (DESC + per_page=1) as the bootstrap so the next call
// only sees changes that happened after we started.
func (m *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("mattermost: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: fetch the newest post (page 0, per_page 1) and
		// use its update_at as the cursor.
		q := url.Values{}
		q.Set("page", "0")
		q.Set("per_page", "1")
		resp, err := m.do(ctx, conn, http.MethodGet, "/api/v4/channels/"+ns.ID+"/posts?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("mattermost: bootstrap posts status=%d", resp.StatusCode)
		}
		var body struct {
			Order []string `json:"order"`
			Posts map[string]struct {
				UpdateAt int64 `json:"update_at"`
			} `json:"posts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("mattermost: decode bootstrap: %w", err)
		}
		var newCursor int64
		for _, id := range body.Order {
			if body.Posts[id].UpdateAt > newCursor {
				newCursor = body.Posts[id].UpdateAt
			}
		}
		if newCursor == 0 {
			newCursor = time.Now().UnixMilli()
		}

		return nil, strconv.FormatInt(newCursor, 10), nil
	}
	q := url.Values{}
	q.Set("since", cursor)
	resp, err := m.do(ctx, conn, http.MethodGet, "/api/v4/channels/"+ns.ID+"/posts?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: mattermost: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("mattermost: delta posts status=%d", resp.StatusCode)
	}
	var body struct {
		Order []string `json:"order"`
		Posts map[string]struct {
			ID       string `json:"id"`
			UpdateAt int64  `json:"update_at"`
			DeleteAt int64  `json:"delete_at"`
		} `json:"posts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("mattermost: decode delta: %w", err)
	}
	newCursor, _ := strconv.ParseInt(cursor, 10, 64)
	changes := make([]connector.DocumentChange, 0, len(body.Order))
	for _, id := range body.Order {
		p := body.Posts[id]
		kind := connector.ChangeUpserted
		if p.DeleteAt > 0 {
			kind = connector.ChangeDeleted
		}
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          p.ID,
				ETag:        strconv.FormatInt(p.UpdateAt, 10),
				UpdatedAt:   time.UnixMilli(p.UpdateAt),
			},
		})
		if p.UpdateAt > newCursor {
			newCursor = p.UpdateAt
		}
	}

	return changes, strconv.FormatInt(newCursor, 10), nil
}

// do is the shared HTTP helper.
func (m *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("mattermost: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mattermost: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the Mattermost connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
