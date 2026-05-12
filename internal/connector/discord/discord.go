// Package discord implements the Discord SourceConnector +
// DeltaSyncer. Discord exposes a REST API at
// https://discord.com/api/v10 authenticated with `Authorization:
// Bot <token>`. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "bot_token": "..." }   // required
package discord

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
	Name = "discord"

	defaultBaseURL = "https://discord.com/api/v10"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	BotToken string `json:"bot_token"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(d *Connector) { d.httpClient = c } }

// WithBaseURL overrides the Discord API base URL — used by tests.
func WithBaseURL(u string) Option { return func(d *Connector) { d.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	d := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(d)
	}

	return d
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	botID    string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (d *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if c.BotToken == "" {
		return fmt.Errorf("%w: bot_token required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /users/@me as the auth check.
func (d *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := d.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.BotToken}

	resp, err := d.do(ctx, conn, http.MethodGet, "/users/@me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord: users/@me status=%d", resp.StatusCode)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("discord: decode users/@me: %w", err)
	}
	conn.botID = body.ID

	return conn, nil
}

// ListNamespaces returns the text channels across guilds the bot
// has joined. We page over guilds + over channels per guild.
func (d *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("discord: bad connection type")
	}

	out := []connector.Namespace{}
	guildAfter := ""
	for {
		q := url.Values{}
		q.Set("limit", "100")
		if guildAfter != "" {
			q.Set("after", guildAfter)
		}
		resp, err := d.do(ctx, conn, http.MethodGet, "/users/@me/guilds?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("discord: list guilds status=%d", resp.StatusCode)
		}
		var guilds []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&guilds); err != nil {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("discord: decode guilds: %w", err)
		}
		_ = resp.Body.Close()
		if len(guilds) == 0 {
			break
		}
		for _, g := range guilds {
			r, err := d.do(ctx, conn, http.MethodGet, "/guilds/"+g.ID+"/channels", nil)
			if err != nil {
				return nil, err
			}
			if r.StatusCode != http.StatusOK {
				_ = r.Body.Close()

				return nil, fmt.Errorf("discord: list channels status=%d", r.StatusCode)
			}
			var channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type int    `json:"type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&channels); err != nil {
				_ = r.Body.Close()

				return nil, fmt.Errorf("discord: decode channels: %w", err)
			}
			_ = r.Body.Close()
			for _, ch := range channels {
				// type 0 = GUILD_TEXT; 5 = GUILD_ANNOUNCEMENT; 15 = GUILD_FORUM.
				if ch.Type != 0 && ch.Type != 5 && ch.Type != 15 {
					continue
				}
				out = append(out, connector.Namespace{
					ID: ch.ID, Name: g.Name + "/" + ch.Name, Kind: "text_channel",
					Metadata: map[string]string{"guild_id": g.ID},
				})
			}
		}
		// /users/@me/guilds paginates by ID; pick the last guild's ID
		// as the next `after` value, terminating when we got fewer
		// than 100 results.
		if len(guilds) < 100 {
			break
		}
		guildAfter = guilds[len(guilds)-1].ID
	}

	return out, nil
}

// msgIterator paginates over /channels/{id}/messages using the
// `before` cursor.
type msgIterator struct {
	d       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	before  string
	err     error
	done    bool
}

func (it *msgIterator) Next(ctx context.Context) bool {
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

func (it *msgIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *msgIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *msgIterator) Close() error { return nil }

func (it *msgIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(pageOr(it.opts.PageSize, 100)))
	if it.before != "" {
		q.Set("before", it.before)
	} else if it.opts.PageToken != "" {
		q.Set("before", it.opts.PageToken)
	}
	resp, err := it.d.do(ctx, it.conn, http.MethodGet, "/channels/"+it.ns.ID+"/messages?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: discord: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("discord: messages status=%d", resp.StatusCode)

		return false
	}
	var msgs []struct {
		ID              string `json:"id"`
		EditedTimestamp string `json:"edited_timestamp"`
		Timestamp       string `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		it.err = fmt.Errorf("discord: decode messages: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range msgs {
		updatedStr := m.Timestamp
		if m.EditedTimestamp != "" {
			updatedStr = m.EditedTimestamp
		}
		updated, _ := time.Parse(time.RFC3339, updatedStr)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.ID,
			ETag:        updatedStr,
			UpdatedAt:   updated,
		})
	}
	if len(msgs) == 0 {
		it.done = true

		return false
	}
	it.before = msgs[len(msgs)-1].ID

	return true
}

// ListDocuments enumerates messages in a channel.
func (d *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("discord: bad connection type")
	}

	return &msgIterator{d: d, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single message by ID.
func (d *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("discord: bad connection type")
	}
	resp, err := d.do(ctx, conn, http.MethodGet, "/channels/"+ref.NamespaceID+"/messages/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord: get message status=%d", resp.StatusCode)
	}
	var body struct {
		ID              string `json:"id"`
		Content         string `json:"content"`
		Timestamp       string `json:"timestamp"`
		EditedTimestamp string `json:"edited_timestamp"`
		Author          struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("discord: decode message: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, body.Timestamp)
	updated := created
	if body.EditedTimestamp != "" {
		updated, _ = time.Parse(time.RFC3339, body.EditedTimestamp)
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/plain",
		Title:     fmt.Sprintf("Message from %s", body.Author.Username),
		Author:    body.Author.Username,
		Size:      int64(len(body.Content)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.Content)),
	}, nil
}

// Subscribe is unsupported — Discord pushes events via the
// gateway WebSocket which is outside the scope of this connector.
func (d *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (d *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /channels/{id}/messages with the `after` cursor.
// The cursor is the highest message ID seen on the previous call.
// Empty cursor on the first call returns the current cursor
// without backfilling — matching the DeltaSyncer contract.
func (d *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("discord: bad connection type")
	}
	q := url.Values{}
	q.Set("limit", "100")
	if cursor != "" {
		q.Set("after", cursor)
	}
	resp, err := d.do(ctx, conn, http.MethodGet, "/channels/"+ns.ID+"/messages?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Surface 429 as connector.ErrRateLimited so the adaptive rate
	// limiter reacts to Discord's strict per-route rate limits during
	// delta sync, mirroring the ListDocuments iterator above.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: discord: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("discord: messages status=%d", resp.StatusCode)
	}
	var msgs []struct {
		ID              string `json:"id"`
		EditedTimestamp string `json:"edited_timestamp"`
		Timestamp       string `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, "", fmt.Errorf("discord: decode messages: %w", err)
	}
	changes := make([]connector.DocumentChange, 0, len(msgs))
	newCursor := cursor
	for _, m := range msgs {
		updatedStr := m.Timestamp
		if m.EditedTimestamp != "" {
			updatedStr = m.EditedTimestamp
		}
		updated, _ := time.Parse(time.RFC3339, updatedStr)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          m.ID,
				ETag:        updatedStr,
				UpdatedAt:   updated,
			},
		})
		// Snowflake IDs are monotonic; lex-compare keeps the
		// largest ID as the cursor without needing to parse.
		if m.ID > newCursor {
			newCursor = m.ID
		}
	}
	if cursor == "" {
		changes = nil
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper.
func (d *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(d.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("discord: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bot "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the Discord connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
