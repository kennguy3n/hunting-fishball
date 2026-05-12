// Package outlook implements the Microsoft 365 mailbox
// SourceConnector + DeltaSyncer against the Microsoft Graph API
// at https://graph.microsoft.com/v1.0. Authentication uses an
// OAuth 2.0 bearer access token with the `Mail.Read` (read-only)
// scope. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "eyJ0..." , // required
//	  "user_id":      "user@x"   // optional, defaults to "me"
//	}
//
// The connector exposes a single synthetic namespace per
// mailbox folder. By default it uses the well-known "inbox"
// folder, but additional folders can be discovered via
// /me/mailFolders. Incremental synchronisation uses Graph's
// `/messages/delta` endpoint with `$deltatoken`.
package outlook

import (
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
	// Name is the registry-visible connector name.
	Name = "outlook"

	defaultBaseURL = "https://graph.microsoft.com/v1.0"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	UserID      string `json:"user_id,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(g *Connector) { g.httpClient = c } }

// WithBaseURL overrides the Graph API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	c := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(c)
	}

	return c
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	user     string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// userPath returns the "/me" or "/users/<id>" base depending on
// whether a user_id was supplied.
func (c *connection) userPath() string {
	if c.user == "" {
		return "/me"
	}

	return "/users/" + c.user
}

// Validate parses and sanity-checks the credential blob.
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

// Connect calls /me (or /users/<id>) as the auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken, user: creds.UserID}
	resp, err := g.do(ctx, conn, http.MethodGet, conn.userPath(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: outlook: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("outlook: %s status=%d", conn.userPath(), resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the mailbox's mail folders.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("outlook: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, conn.userPath()+"/mailFolders", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: outlook: folders status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("outlook: folders status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("outlook: decode folders: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body.Value))
	for _, f := range body.Value {
		out = append(out, connector.Namespace{ID: f.ID, Name: f.DisplayName, Kind: "folder"})
	}

	return out, nil
}

type messageIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	nextURL string
	first   bool
	err     error
	done    bool
}

func (it *messageIterator) Next(ctx context.Context) bool {
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

func (it *messageIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *messageIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *messageIterator) Close() error { return nil }

func (it *messageIterator) fetchPage(ctx context.Context) bool {
	var path string
	if !it.first {
		it.first = true
		path = it.conn.userPath() + "/mailFolders/" + it.ns.ID + "/messages?$top=50"
	} else if it.nextURL != "" {
		path = it.nextURL
	} else {
		it.done = true

		return false
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: outlook: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("outlook: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Value []struct {
			ID                   string `json:"id"`
			LastModifiedDateTime string `json:"lastModifiedDateTime"`
		} `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("outlook: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range body.Value {
		updated, _ := time.Parse(time.RFC3339, m.LastModifiedDateTime)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.ID,
			UpdatedAt:   updated,
		})
	}
	if body.NextLink == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.nextURL = body.NextLink
	}

	return true
}

// ListDocuments enumerates messages in the folder namespace.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("outlook: bad connection type")
	}

	return &messageIterator{g: g, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single message in metadata + bodyPreview form.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("outlook: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, conn.userPath()+"/messages/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: outlook: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("outlook: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("outlook: read body: %w", err)
	}
	var body struct {
		ID                   string `json:"id"`
		Subject              string `json:"subject"`
		BodyPreview          string `json:"bodyPreview"`
		LastModifiedDateTime string `json:"lastModifiedDateTime"`
		ReceivedDateTime     string `json:"receivedDateTime"`
		From                 struct {
			EmailAddress struct {
				Address string `json:"address"`
				Name    string `json:"name"`
			} `json:"emailAddress"`
		} `json:"from"`
	}
	_ = json.Unmarshal(raw, &body)
	title := body.Subject
	if title == "" {
		title = body.BodyPreview
	}
	if len(title) > 200 {
		title = title[:200]
	}
	updated, _ := time.Parse(time.RFC3339, body.LastModifiedDateTime)
	received, _ := time.Parse(time.RFC3339, body.ReceivedDateTime)
	if updated.IsZero() {
		updated = received
	}

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: ref.NamespaceID, ID: body.ID, UpdatedAt: updated},
		MIMEType:  "application/json",
		Title:     title,
		Author:    body.From.EmailAddress.Address,
		Size:      int64(len(raw)),
		CreatedAt: received,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"folder_id": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Graph push notifications use the
// `/subscriptions` resource which is out of scope for stdlib
// ingestion. Callers fall back to DeltaSync.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses Graph's `/messages/delta` endpoint. The cursor
// is the @odata.deltaLink returned from the previous call. Empty
// cursor bootstraps with `?$deltatoken=latest` and returns the
// fresh deltaLink without backfilling history.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("outlook: bad connection type")
	}
	var path string
	if cursor == "" {
		path = conn.userPath() + "/mailFolders/" + ns.ID + "/messages/delta?$deltatoken=latest"
	} else {
		path = cursor
	}
	resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: outlook: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("outlook: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID                   string    `json:"id"`
			Removed              *struct{} `json:"@removed"`
			LastModifiedDateTime string    `json:"lastModifiedDateTime"`
		} `json:"value"`
		NextLink  string `json:"@odata.nextLink"`
		DeltaLink string `json:"@odata.deltaLink"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("outlook: decode delta: %w", err)
	}
	if cursor == "" {
		next := body.DeltaLink
		if next == "" {
			next = body.NextLink
		}

		return nil, next, nil
	}
	changes := make([]connector.DocumentChange, 0, len(body.Value))
	for _, m := range body.Value {
		kind := connector.ChangeUpserted
		if m.Removed != nil {
			kind = connector.ChangeDeleted
		}
		updated, _ := time.Parse(time.RFC3339, m.LastModifiedDateTime)
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: m.ID, UpdatedAt: updated},
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

// do is the shared HTTP helper. Absolute URLs are routed
// directly; relative paths are joined with baseURL. Graph's
// @odata.nextLink / @odata.deltaLink are absolute.
func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		target = path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("outlook: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("outlook: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Outlook connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
