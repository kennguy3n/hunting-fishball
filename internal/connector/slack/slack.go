// Package slack implements the Slack SourceConnector + WebhookReceiver.
//
// Like the Google Drive connector, this package talks to slack.com REST
// endpoints with stdlib net/http. Unit tests inject httptest-backed
// HTTP clients. Future phases can swap to slack-go/slack without
// touching SourceConnector callers.
//
// Credentials must be a JSON blob:
//
//	{
//	  "bot_token":     "xoxb-...",     // required
//	  "signing_secret": "..."           // optional, used by HandleWebhook
//	}
package slack

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
	Name = "slack"

	defaultBaseURL = "https://slack.com/api"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	BotToken      string `json:"bot_token"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	signingSecret string

	// timestampSkew bounds how far off the request timestamp can
	// be from now() before VerifyWebhookRequest rejects it.
	// Defaults to 5 minutes (Slack's recommended value).
	timestampSkew time.Duration

	// nowFn is used by tests to inject a fixed wall clock so the
	// timestamp-skew check is deterministic.
	nowFn func() time.Time
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient sets the underlying HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Connector) { s.httpClient = c }
}

// WithBaseURL overrides the Slack API base URL.
func WithBaseURL(u string) Option {
	return func(s *Connector) { s.baseURL = u }
}

// WithSigningSecret enables Slack v0 signature verification on
// HandleWebhook callers that route through VerifyWebhookRequest.
// Empty disables verification (development default).
func WithSigningSecret(s string) Option {
	return func(c *Connector) { c.signingSecret = s }
}

// WithTimestampSkew overrides the maximum allowed clock skew
// between Slack and the local system. Defaults to 5 minutes.
func WithTimestampSkew(d time.Duration) Option {
	return func(c *Connector) { c.timestampSkew = d }
}

// WithNow injects a wall-clock function for tests.
func WithNow(fn func() time.Time) Option {
	return func(c *Connector) { c.nowFn = fn }
}

// New constructs a Connector.
func New(opts ...Option) *Connector {
	s := &Connector{
		httpClient:    &http.Client{Timeout: 30 * time.Second},
		baseURL:       defaultBaseURL,
		timestampSkew: 5 * time.Minute,
		nowFn:         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.timestampSkew <= 0 {
		s.timestampSkew = 5 * time.Minute
	}
	if s.nowFn == nil {
		s.nowFn = time.Now
	}

	return s
}

// VerifyWebhookRequest validates the Slack v0 signature scheme
// against the configured signing secret. Slack signs every request
// as
//
//	v0=hex(HMAC_SHA256(secret, "v0:"+ts +":"+ body))
//
// using the headers `X-Slack-Signature` and
// `X-Slack-Request-Timestamp`. The timestamp is also bounded by
// timestampSkew (default 5 min) to make replay attacks harder.
//
// When signingSecret is empty the function returns nil so
// development environments can skip verification.
func (s *Connector) VerifyWebhookRequest(headers map[string][]string, payload []byte) error {
	if s.signingSecret == "" {
		return nil
	}
	sig := strings.TrimSpace(connector.FirstHeader(headers, "X-Slack-Signature"))
	ts := strings.TrimSpace(connector.FirstHeader(headers, "X-Slack-Request-Timestamp"))
	if sig == "" || ts == "" {
		return connector.ErrWebhookSignatureMissing
	}
	parsedTS, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return connector.ErrWebhookSignatureInvalid
	}
	if delta := s.nowFn().Unix() - parsedTS; delta > int64(s.timestampSkew.Seconds()) || delta < -int64(s.timestampSkew.Seconds()) {
		return connector.ErrWebhookSignatureInvalid
	}
	mac := hmac.New(sha256.New, []byte(s.signingSecret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(ts))
	mac.Write([]byte{':'})
	mac.Write(payload)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return connector.ErrWebhookSignatureInvalid
	}
	return nil
}

// connection holds the per-call state derived from ConnectorConfig.
type connection struct {
	tenantID string
	sourceID string
	token    string
	teamID   string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (s *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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

// Connect calls auth.test as a cheap auth check.
func (s *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := s.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}

	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.BotToken}
	resp, err := s.do(ctx, conn, http.MethodGet, "/auth.test", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		TeamID string `json:"team_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("slack: decode auth.test: %w", err)
	}
	if !body.OK {
		return nil, fmt.Errorf("slack: auth.test failed: %s", body.Error)
	}
	conn.teamID = body.TeamID

	return conn, nil
}

// ListNamespaces returns the channels the bot is in.
func (s *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("slack: bad connection type")
	}

	q := url.Values{}
	q.Set("types", "public_channel,private_channel")
	q.Set("limit", "200")
	q.Set("exclude_archived", "true")

	out := []connector.Namespace{}
	for {
		resp, err := s.do(ctx, conn, http.MethodGet, "/conversations.list?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var body struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Channels []struct {
				ID         string `json:"id"`
				Name       string `json:"name"`
				IsPrivate  bool   `json:"is_private"`
				IsArchived bool   `json:"is_archived"`
			} `json:"channels"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("slack: decode channels: %w", err)
		}
		_ = resp.Body.Close()
		if !body.OK {
			return nil, fmt.Errorf("slack: conversations.list: %s", body.Error)
		}
		for _, ch := range body.Channels {
			kind := "public_channel"
			if ch.IsPrivate {
				kind = "private_channel"
			}
			out = append(out, connector.Namespace{
				ID: ch.ID, Name: ch.Name, Kind: kind,
			})
		}
		if body.ResponseMetadata.NextCursor == "" {
			break
		}
		q.Set("cursor", body.ResponseMetadata.NextCursor)
	}

	return out, nil
}

// msgIterator paginates over conversations.history.
type msgIterator struct {
	s       *Connector
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
		q.Set("oldest", strconv.FormatFloat(float64(it.opts.Since.Unix()), 'f', 6, 64))
	}

	resp, err := it.s.do(ctx, it.conn, http.MethodGet, "/conversations.history?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: slack: conversations.history status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}

	var body struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			Ts   string `json:"ts"`
			User string `json:"user"`
		} `json:"messages"`
		HasMore          bool `json:"has_more"`
		ResponseMetadata struct {
			NextCursor string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("slack: decode history: %w", err)

		return false
	}
	if !body.OK {
		// Slack often signals rate-limit via OK=false with error="ratelimited"
		// rather than HTTP 429. Surface that explicitly to the adaptive
		// rate limiter.
		if body.Error == "ratelimited" {
			it.err = fmt.Errorf("%w: slack: conversations.history: %s", connector.ErrRateLimited, body.Error)

			return false
		}
		it.err = fmt.Errorf("slack: conversations.history: %s", body.Error)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range body.Messages {
		updated := slackTSToTime(m.Ts)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.Ts,
			ETag:        m.Ts,
			UpdatedAt:   updated,
		})
	}
	if body.ResponseMetadata.NextCursor == "" || !body.HasMore {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.ResponseMetadata.NextCursor
	}

	return true
}

// ListDocuments enumerates messages in the channel.
func (s *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("slack: bad connection type")
	}

	return &msgIterator{s: s, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument returns the single message identified by ref.ID (Slack ts).
func (s *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("slack: bad connection type")
	}

	q := url.Values{}
	q.Set("channel", ref.NamespaceID)
	q.Set("latest", ref.ID)
	q.Set("oldest", ref.ID)
	q.Set("inclusive", "true")
	q.Set("limit", "1")

	resp, err := s.do(ctx, conn, http.MethodGet, "/conversations.history?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Messages []struct {
			Ts   string `json:"ts"`
			User string `json:"user"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("slack: decode message: %w", err)
	}
	if !body.OK {
		return nil, fmt.Errorf("slack: conversations.history: %s", body.Error)
	}
	if len(body.Messages) == 0 {
		return nil, fmt.Errorf("slack: message %s not found", ref.ID)
	}
	m := body.Messages[0]
	updated := slackTSToTime(m.Ts)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/plain",
		Title:     fmt.Sprintf("Message from %s", m.User),
		Author:    m.User,
		Size:      int64(len(m.Text)),
		CreatedAt: updated,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(m.Text)),
		Metadata:  map[string]string{"channel_id": ref.NamespaceID, "ts": m.Ts, "team_id": conn.teamID},
	}, nil
}

// Subscribe is unsupported — Slack pushes change events via the
// HandleWebhook path on the WebhookReceiver interface.
func (s *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (s *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// HandleWebhook decodes a Slack Events API payload into a list of
// DocumentChange values.
//
// Supported event types:
//   - "url_verification" — returns no changes (caller responds with the
//     challenge directly).
//   - "event_callback" wrapping a "message" event — emitted as
//     ChangeUpserted with the channel_id as the NamespaceID and the
//     ts as the document ID.
//   - "event_callback" wrapping a "message_deleted" event — emitted as
//     ChangeDeleted.
func (s *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	if len(payload) == 0 {
		return nil, errors.New("slack: empty webhook payload")
	}
	var env struct {
		Type      string          `json:"type"`
		Challenge string          `json:"challenge"`
		Event     json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("slack: decode webhook: %w", err)
	}
	switch env.Type {
	case "url_verification":
		return nil, nil
	case "event_callback":
		// fallthrough below
	default:
		return nil, nil
	}

	var ev struct {
		Type        string `json:"type"`
		Channel     string `json:"channel"`
		Ts          string `json:"ts"`
		DeletedTs   string `json:"deleted_ts"`
		Subtype     string `json:"subtype"`
		PreviousMsg struct {
			Ts string `json:"ts"`
		} `json:"previous_message"`
	}
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		return nil, fmt.Errorf("slack: decode inner event: %w", err)
	}
	if ev.Type != "message" {
		return nil, nil
	}

	if ev.Subtype == "message_deleted" {
		ts := ev.DeletedTs
		if ts == "" {
			ts = ev.PreviousMsg.Ts
		}
		if ts == "" {
			return nil, nil
		}

		return []connector.DocumentChange{{
			Kind: connector.ChangeDeleted,
			Ref:  connector.DocumentRef{NamespaceID: ev.Channel, ID: ts, ETag: ts, UpdatedAt: slackTSToTime(ts)},
		}}, nil
	}

	if ev.Ts == "" || ev.Channel == "" {
		return nil, nil
	}

	return []connector.DocumentChange{{
		Kind: connector.ChangeUpserted,
		Ref:  connector.DocumentRef{NamespaceID: ev.Channel, ID: ev.Ts, ETag: ev.Ts, UpdatedAt: slackTSToTime(ev.Ts)},
	}}, nil
}

// WebhookPath implements connector.WebhookReceiver.
func (s *Connector) WebhookPath() string { return "/slack" }

// do is the shared HTTP path.
func (s *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(s.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("slack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slack: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func slackTSToTime(ts string) time.Time {
	dot := strings.IndexByte(ts, '.')
	if dot < 0 {
		secs, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return time.Time{}
		}

		return time.Unix(secs, 0).UTC()
	}
	secs, err := strconv.ParseInt(ts[:dot], 10, 64)
	if err != nil {
		return time.Time{}
	}

	return time.Unix(secs, 0).UTC()
}

// pageOr returns v when the caller supplied a positive page size and
// otherwise falls back to the connector default. ListOpts.PageSize
// documents "zero means connector default", so this must NOT clamp
// caller-supplied values upward — that would silently override the
// caller's preference.
func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the Slack connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
