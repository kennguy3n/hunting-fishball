// Package kchat implements the KChat SourceConnector + DeltaSyncer +
// WebhookReceiver. KChat is the internal chat platform listed as a
// Phase-1 connector in docs/PROPOSAL.md §4. It is modelled here as a
// generic REST chat API with channels, messages, and webhook events
// — the same shape as the Slack connector but with a configurable
// base URL so on-prem KChat deployments can point at their own
// internal endpoint.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_token":      "kch-...",  // required
//	  "signing_secret": "..."        // optional, used by VerifyWebhookRequest
//	}
//
// Settings may include `base_url` to override the default KChat
// endpoint at https://kchat.internal/api/v1.
package kchat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	Name = "kchat"

	defaultBaseURL = "https://kchat.internal/api/v1"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIToken      string `json:"api_token"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer +
// WebhookReceiver against the KChat REST API.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	signingSecret string
	timestampSkew time.Duration
	nowFn         func() time.Time
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(k *Connector) { k.httpClient = c } }

// WithBaseURL overrides the KChat API base URL — used by tests.
func WithBaseURL(u string) Option { return func(k *Connector) { k.baseURL = u } }

// WithSigningSecret enables KChat webhook signature verification.
// Empty disables verification (development default).
func WithSigningSecret(s string) Option { return func(k *Connector) { k.signingSecret = s } }

// WithTimestampSkew overrides the maximum allowed clock skew on
// webhook verification. Defaults to 5 minutes.
func WithTimestampSkew(d time.Duration) Option { return func(k *Connector) { k.timestampSkew = d } }

// WithNow injects a wall-clock function for tests.
func WithNow(fn func() time.Time) Option { return func(k *Connector) { k.nowFn = fn } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	k := &Connector{
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		baseURL:       defaultBaseURL,
		timestampSkew: 5 * time.Minute,
		nowFn:         time.Now,
	}
	for _, opt := range opts {
		opt(k)
	}
	if k.timestampSkew <= 0 {
		k.timestampSkew = 5 * time.Minute
	}
	if k.nowFn == nil {
		k.nowFn = time.Now
	}

	return k
}

// connection holds the per-call state derived from ConnectorConfig.
type connection struct {
	tenantID string
	sourceID string
	token    string
	userID   string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (k *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if c.APIToken == "" {
		return fmt.Errorf("%w: api_token required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /users.me as a cheap auth check.
func (k *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := k.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}

	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.APIToken}
	resp, err := k.do(ctx, conn, http.MethodGet, "/users.me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kchat: auth check failed: status=%d", resp.StatusCode)
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("kchat: decode users.me: %w", err)
	}
	conn.userID = body.ID

	return conn, nil
}

// ListNamespaces returns the channels visible to the bot.
func (k *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("kchat: bad connection type")
	}

	q := url.Values{}
	q.Set("limit", "200")

	out := []connector.Namespace{}
	for {
		resp, err := k.do(ctx, conn, http.MethodGet, "/channels.list?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("kchat: channels.list status=%d", resp.StatusCode)
		}
		var body struct {
			Channels []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				IsPrivate bool   `json:"is_private"`
			} `json:"channels"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("kchat: decode channels: %w", err)
		}
		_ = resp.Body.Close()
		for _, ch := range body.Channels {
			kind := "public_channel"
			if ch.IsPrivate {
				kind = "private_channel"
			}
			out = append(out, connector.Namespace{
				ID: ch.ID, Name: ch.Name, Kind: kind,
			})
		}
		if body.NextCursor == "" {
			break
		}
		q.Set("cursor", body.NextCursor)
	}

	return out, nil
}

// msgIterator paginates over /channels.history.
type msgIterator struct {
	k       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
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
	q.Set("channel", it.ns.ID)
	q.Set("limit", strconv.Itoa(pageOr(it.opts.PageSize, 100)))
	if it.cursor != "" {
		q.Set("cursor", it.cursor)
	} else if it.opts.PageToken != "" {
		q.Set("cursor", it.opts.PageToken)
	}
	if !it.opts.Since.IsZero() {
		q.Set("since", it.opts.Since.UTC().Format(time.RFC3339))
	}
	resp, err := it.k.do(ctx, it.conn, http.MethodGet, "/channels.history?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: kchat: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("kchat: channels.history status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Messages []struct {
			ID        string `json:"id"`
			Author    string `json:"author"`
			UpdatedAt string `json:"updated_at"`
		} `json:"messages"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("kchat: decode history: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range body.Messages {
		updated, _ := time.Parse(time.RFC3339, m.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.ID,
			ETag:        m.ID,
			UpdatedAt:   updated,
		})
	}
	if body.NextCursor == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.NextCursor
	}

	return true
}

// ListDocuments enumerates messages in a channel.
func (k *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("kchat: bad connection type")
	}

	return &msgIterator{k: k, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument returns the single message identified by ref.ID.
func (k *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("kchat: bad connection type")
	}

	q := url.Values{}
	q.Set("channel", ref.NamespaceID)
	q.Set("id", ref.ID)

	resp, err := k.do(ctx, conn, http.MethodGet, "/messages.get?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kchat: messages.get status=%d", resp.StatusCode)
	}

	var body struct {
		ID        string `json:"id"`
		Author    string `json:"author"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("kchat: decode message: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, body.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, body.UpdatedAt)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/plain",
		Title:     fmt.Sprintf("Message from %s", body.Author),
		Author:    body.Author,
		Size:      int64(len(body.Text)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.Text)),
		Metadata:  map[string]string{"channel_id": ref.NamespaceID, "message_id": body.ID},
	}, nil
}

// Subscribe is unsupported — KChat pushes change events via the
// HandleWebhook path on the WebhookReceiver interface.
func (k *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (k *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /channels.changes with the previous cursor and
// returns the new cursor along with any document changes. An empty
// cursor on the first call returns the current cursor without
// backfilling history, matching the DeltaSyncer contract.
func (k *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("kchat: bad connection type")
	}

	q := url.Values{}
	q.Set("channel", ns.ID)
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := k.do(ctx, conn, http.MethodGet, "/channels.changes?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("kchat: channels.changes status=%d", resp.StatusCode)
	}

	var body struct {
		Cursor  string `json:"cursor"`
		Changes []struct {
			Kind      string `json:"kind"`
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		} `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("kchat: decode changes: %w", err)
	}

	out := make([]connector.DocumentChange, 0, len(body.Changes))
	for _, ch := range body.Changes {
		updated, _ := time.Parse(time.RFC3339, ch.UpdatedAt)
		kind := connector.ChangeUpserted
		if ch.Kind == "deleted" {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          ch.ID,
				ETag:        ch.ID,
				UpdatedAt:   updated,
			},
		})
	}

	return out, body.Cursor, nil
}

// HandleWebhook decodes a KChat webhook payload into a list of
// DocumentChange values.
//
// Supported event types:
//   - "url_verification"   — returns no changes (caller responds with
//     the challenge directly).
//   - "message.created"    — emitted as ChangeUpserted.
//   - "message.updated"    — emitted as ChangeUpserted.
//   - "message.deleted"    — emitted as ChangeDeleted.
func (k *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	if len(payload) == 0 {
		return nil, errors.New("kchat: empty webhook payload")
	}
	var env struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		Message struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("kchat: decode webhook: %w", err)
	}
	switch env.Type {
	case "url_verification":
		return nil, nil
	case "message.created", "message.updated":
		if env.Message.ID == "" || env.Channel == "" {
			return nil, nil
		}
		updated, _ := time.Parse(time.RFC3339, env.Message.UpdatedAt)

		return []connector.DocumentChange{{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: env.Channel, ID: env.Message.ID, ETag: env.Message.ID, UpdatedAt: updated},
		}}, nil
	case "message.deleted":
		if env.Message.ID == "" || env.Channel == "" {
			return nil, nil
		}

		return []connector.DocumentChange{{
			Kind: connector.ChangeDeleted,
			Ref:  connector.DocumentRef{NamespaceID: env.Channel, ID: env.Message.ID, ETag: env.Message.ID},
		}}, nil
	}

	return nil, nil
}

// WebhookPath implements connector.WebhookReceiver.
func (k *Connector) WebhookPath() string { return "/kchat" }

// VerifyWebhookRequest validates a KChat webhook signature. KChat
// signs payloads using the same scheme as Slack:
//
//	v1=hex(HMAC_SHA256(secret, "v1:"+ts+":"+body))
//
// using the headers `X-KChat-Signature` and
// `X-KChat-Request-Timestamp`. When signingSecret is empty
// verification is skipped (development default).
func (k *Connector) VerifyWebhookRequest(headers map[string][]string, payload []byte) error {
	if k.signingSecret == "" {
		return nil
	}
	sig := strings.TrimSpace(connector.FirstHeader(headers, "X-KChat-Signature"))
	ts := strings.TrimSpace(connector.FirstHeader(headers, "X-KChat-Request-Timestamp"))
	if sig == "" || ts == "" {
		return connector.ErrWebhookSignatureMissing
	}
	parsedTS, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return connector.ErrWebhookSignatureInvalid
	}
	if delta := k.nowFn().Unix() - parsedTS; delta > int64(k.timestampSkew.Seconds()) || delta < -int64(k.timestampSkew.Seconds()) {
		return connector.ErrWebhookSignatureInvalid
	}
	mac := hmac.New(sha256.New, []byte(k.signingSecret))
	mac.Write([]byte("v1:"))
	mac.Write([]byte(ts))
	mac.Write([]byte{':'})
	mac.Write(payload)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return connector.ErrWebhookSignatureInvalid
	}

	return nil
}

// do is the shared HTTP helper.
func (k *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(k.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("kchat: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kchat: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// pageOr returns v when the caller supplied a positive page size and
// otherwise falls back to the connector default. Matches the
// pageOr helper used in other connectors.
func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the KChat connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
