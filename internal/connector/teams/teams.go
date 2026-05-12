// Package teams implements the Microsoft Teams SourceConnector
// against the Microsoft Graph API.
//
// We list joined teams, then channels per team, and enumerate
// channel messages. The optional DeltaSyncer capability uses Graph
// `/messages/delta`. The optional WebhookReceiver decodes Microsoft
// Graph change notifications.
//
// Credentials must be a JSON blob with `access_token`. The control
// plane decrypts internal/credential storage before supplying bytes
// to Connect.
package teams

import (
	"bytes"
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
	// Name is the registry-visible connector name.
	Name           = "teams"
	defaultBaseURL = "https://graph.microsoft.com/v1.0"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	webhookSecret string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// WithWebhookSecret enables signature verification on incoming
// webhooks. Microsoft Graph change notifications include a
// `clientState` field in the body that mirrors the secret the caller
// registered with the subscription; for the platform's purposes we
// expect callers to forward that value as a bearer token in the
// `Authorization` header (Phase 8 / Task 9).
func WithWebhookSecret(secret string) Option {
	return func(g *Connector) { g.webhookSecret = secret }
}

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: defaultBaseURL}
	for _, o := range opts {
		o(c)
	}
	return c
}

// VerifyWebhookRequest validates the Teams subscription `clientState`
// (carried in `Authorization`) against the configured webhook secret.
// Implements connector.WebhookVerifier.
func (g *Connector) VerifyWebhookRequest(headers map[string][]string, _ []byte) error {
	if g.webhookSecret == "" {
		return nil
	}
	return connector.VerifyTokenEqual(g.webhookSecret, connector.FirstHeader(headers, "Authorization"))
}

type connection struct {
	tenantID, sourceID, accessToken string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate checks cfg for the Teams connector.
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

// Connect performs an auth check via `/me`.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, accessToken: c.AccessToken}
	resp, err := g.do(ctx, conn, http.MethodGet, "/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("teams: auth status=%d", resp.StatusCode)
	}
	return conn, nil
}

// ListNamespaces enumerates team channels — Namespace.ID is
// "{teamId}/{channelId}" so list/fetch can recover both.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("teams: bad connection type")
	}
	teamsResp, err := g.do(ctx, conn, http.MethodGet, "/me/joinedTeams", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = teamsResp.Body.Close() }()
	if teamsResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("teams: list teams status=%d", teamsResp.StatusCode)
	}
	var teamsBody struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.NewDecoder(teamsResp.Body).Decode(&teamsBody); err != nil {
		return nil, fmt.Errorf("teams: decode teams: %w", err)
	}
	out := []connector.Namespace{}
	for _, t := range teamsBody.Value {
		chResp, err := g.do(ctx, conn, http.MethodGet, "/teams/"+url.PathEscape(t.ID)+"/channels", nil)
		if err != nil {
			return nil, err
		}
		var chBody struct {
			Value []struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"value"`
		}
		if err := json.NewDecoder(chResp.Body).Decode(&chBody); err != nil {
			_ = chResp.Body.Close()
			return nil, fmt.Errorf("teams: decode channels: %w", err)
		}
		_ = chResp.Body.Close()
		for _, ch := range chBody.Value {
			out = append(out, connector.Namespace{
				ID:   t.ID + "/" + ch.ID,
				Name: t.DisplayName + "#" + ch.DisplayName,
				Kind: "channel",
				Metadata: map[string]string{
					"team_id":    t.ID,
					"channel_id": ch.ID,
				},
			})
		}
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

func splitChannel(nsID string) (team, channel string) {
	parts := strings.SplitN(nsID, "/", 2)
	if len(parts) != 2 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (it *docIterator) fetchPage(ctx context.Context) bool {
	team, ch := splitChannel(it.ns.ID)
	if ch == "" {
		it.err = errors.New("teams: namespace must encode team_id/channel_id")
		return false
	}
	path := "/teams/" + url.PathEscape(team) + "/channels/" + url.PathEscape(ch) + "/messages"
	if it.cursor != "" {
		path = it.cursor
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: teams: list messages status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("teams: list messages status=%d", resp.StatusCode)
		return false
	}
	var body struct {
		Value []struct {
			ID                   string `json:"id"`
			LastModifiedDateTime string `json:"lastModifiedDateTime"`
			ETag                 string `json:"etag"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("teams: decode: %w", err)
		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range body.Value {
		modified, _ := time.Parse(time.RFC3339, m.LastModifiedDateTime)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.ID,
			ETag:        m.ETag,
			UpdatedAt:   modified,
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

// ListDocuments enumerates channel messages.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("teams: bad connection type")
	}
	return &docIterator{g: g, conn: conn, ns: ns}, nil
}

// FetchDocument returns the message body.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("teams: bad connection type")
	}
	team, ch := splitChannel(ref.NamespaceID)
	if ch == "" {
		return nil, errors.New("teams: namespace must encode team_id/channel_id")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/teams/"+url.PathEscape(team)+"/channels/"+url.PathEscape(ch)+"/messages/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("teams: get message status=%d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var meta struct {
		Subject              string `json:"subject"`
		CreatedDateTime      string `json:"createdDateTime"`
		LastModifiedDateTime string `json:"lastModifiedDateTime"`
		From                 struct {
			User struct {
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"from"`
	}
	_ = json.Unmarshal(raw, &meta)
	created, _ := time.Parse(time.RFC3339, meta.CreatedDateTime)
	modified, _ := time.Parse(time.RFC3339, meta.LastModifiedDateTime)
	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     meta.Subject,
		Author:    meta.From.User.DisplayName,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(bytes.NewReader(raw)),
		Metadata:  map[string]string{"team_id": team, "channel_id": ch, "message_id": ref.ID},
	}, nil
}

// Subscribe returns ErrNotSupported (use webhooks or DeltaSync).
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync calls /teams/{tid}/channels/{cid}/messages/delta.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("teams: bad connection type")
	}
	team, ch := splitChannel(ns.ID)
	if ch == "" {
		return nil, "", errors.New("teams: namespace must encode team_id/channel_id")
	}
	path := "/teams/" + url.PathEscape(team) + "/channels/" + url.PathEscape(ch) + "/messages/delta"
	if cursor != "" {
		path = strings.TrimPrefix(cursor, g.baseURL)
	}
	resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("teams: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID                   string    `json:"id"`
			Deleted              *struct{} `json:"deletedDateTime"`
			LastModifiedDateTime string    `json:"lastModifiedDateTime"`
		} `json:"value"`
		DeltaLink string `json:"@odata.deltaLink"`
		NextLink  string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("teams: decode delta: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Value))
	for _, m := range body.Value {
		kind := connector.ChangeUpserted
		if m.Deleted != nil {
			kind = connector.ChangeDeleted
		}
		modified, _ := time.Parse(time.RFC3339, m.LastModifiedDateTime)
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: m.ID, UpdatedAt: modified},
		})
	}
	next := body.DeltaLink
	if next == "" {
		next = body.NextLink
	}
	if next == "" {
		next = cursor
	}
	return out, next, nil
}

// HandleWebhook decodes a Microsoft Graph change-notification batch.
func (g *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	var body struct {
		Value []struct {
			ChangeType string `json:"changeType"`
			Resource   string `json:"resource"`
		} `json:"value"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("teams: webhook decode: %w", err)
	}
	out := make([]connector.DocumentChange, 0, len(body.Value))
	for _, v := range body.Value {
		// Resource is e.g. "teams('tid')/channels('cid')/messages('mid')".
		teamID, chID, msgID := parseTeamsResource(v.Resource)
		if msgID == "" {
			continue
		}
		kind := connector.ChangeUpserted
		if strings.EqualFold(v.ChangeType, "deleted") {
			kind = connector.ChangeDeleted
		}
		out = append(out, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: teamID + "/" + chID, ID: msgID},
		})
	}
	return out, nil
}

// WebhookPath is the path suffix the platform mounts the receiver at.
func (g *Connector) WebhookPath() string { return "/teams" }

func parseTeamsResource(res string) (team, channel, message string) {
	res = strings.ReplaceAll(res, "'", "")
	parts := strings.Split(res, "/")
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "teams("):
			team = strings.TrimSuffix(strings.TrimPrefix(p, "teams("), ")")
		case strings.HasPrefix(p, "channels("):
			channel = strings.TrimSuffix(strings.TrimPrefix(p, "channels("), ")")
		case strings.HasPrefix(p, "messages("):
			message = strings.TrimSuffix(strings.TrimPrefix(p, "messages("), ")")
		}
	}
	return team, channel, message
}

func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("teams: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("teams: %s %s: %w", method, target, err)
	}
	return resp, nil
}

// Register hooks the Teams connector into the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
