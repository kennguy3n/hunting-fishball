// Package gmail implements the Gmail SourceConnector +
// DeltaSyncer against the Gmail REST API at
// https://gmail.googleapis.com/gmail/v1. Authentication uses an
// OAuth bearer access token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "access_token": "ya29.a0..." }   // required
package gmail

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
	Name = "gmail"

	defaultBaseURL  = "https://gmail.googleapis.com/gmail/v1"
	defaultPageSize = 100
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
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

// WithBaseURL overrides the Gmail API base URL — used by tests.
func WithBaseURL(u string) Option { return func(g *Connector) { g.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	g := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(g)
	}

	return g
}

type connection struct {
	tenantID string
	sourceID string
	token    string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

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

// Connect calls /users/me/profile as the auth check.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken}
	resp, err := g.do(ctx, conn, http.MethodGet, "/users/me/profile", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gmail: /profile status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the user's labels as namespaces — but
// the most common ingestion target is the synthetic "INBOX"
// label, plus user-defined labels.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gmail: bad connection type")
	}
	resp, err := g.do(ctx, conn, http.MethodGet, "/users/me/labels", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gmail: labels status=%d", resp.StatusCode)
	}
	var body struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("gmail: decode labels: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body.Labels))
	for _, l := range body.Labels {
		out = append(out, connector.Namespace{
			ID: l.ID, Name: l.Name, Kind: "label",
			Metadata: map[string]string{"label_type": l.Type},
		})
	}

	return out, nil
}

// messageIterator paginates messages with the given label filter.
type messageIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	token   string
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
	if !it.first {
		it.first = true
	} else if it.token == "" {
		it.done = true

		return false
	}
	q := url.Values{}
	q.Set("labelIds", it.ns.ID)
	q.Set("maxResults", strconv.Itoa(defaultPageSize))
	if it.token != "" {
		q.Set("pageToken", it.token)
	}
	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/users/me/messages?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("gmail: messages list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"messages"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("gmail: decode messages: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, m := range body.Messages {
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          m.ID,
		})
	}
	it.token = body.NextPageToken
	if it.token == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	}

	return true
}

// ListDocuments enumerates messages in the label namespace.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gmail: bad connection type")
	}

	return &messageIterator{g: g, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single message in metadata + snippet form.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gmail: bad connection type")
	}
	q := url.Values{}
	q.Set("format", "metadata")
	q.Set("metadataHeaders", "Subject")
	q.Add("metadataHeaders", "From")
	resp, err := g.do(ctx, conn, http.MethodGet, "/users/me/messages/"+ref.ID+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gmail: fetch message status=%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gmail: read body: %w", err)
	}
	var body struct {
		ID           string `json:"id"`
		InternalDate string `json:"internalDate"`
		Snippet      string `json:"snippet"`
		HistoryID    string `json:"historyId"`
		Payload      struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(raw, &body)
	var subject, from string
	for _, h := range body.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "subject":
			subject = h.Value
		case "from":
			from = h.Value
		}
	}
	if subject == "" {
		subject = body.Snippet
	}
	if len(subject) > 200 {
		subject = subject[:200]
	}
	ms, _ := strconv.ParseInt(body.InternalDate, 10, 64)
	updated := time.UnixMilli(ms)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: ref.NamespaceID, ID: body.ID, ETag: body.HistoryID, UpdatedAt: updated},
		MIMEType:  "application/json",
		Title:     subject,
		Author:    from,
		Size:      int64(len(raw)),
		CreatedAt: updated,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"history_id": body.HistoryID, "label_id": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Gmail push notifications use Cloud
// Pub/Sub which is out of scope for stdlib ingestion.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses /users/me/history with startHistoryId. The
// historyId is a monotonic positive int64 stored as the cursor.
// Empty cursor on the first call returns the user's current
// historyId from /profile without backfilling history.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("gmail: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: profile.historyId is the current point in time.
		resp, err := g.do(ctx, conn, http.MethodGet, "/users/me/profile", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("gmail: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			HistoryID string `json:"historyId"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("gmail: decode bootstrap: %w", err)
		}
		if body.HistoryID == "" {
			body.HistoryID = "1"
		}

		return nil, body.HistoryID, nil
	}
	q := url.Values{}
	q.Set("startHistoryId", cursor)
	q.Set("labelId", ns.ID)
	q.Set("historyTypes", "messageAdded")
	q.Add("historyTypes", "messageDeleted")
	resp, err := g.do(ctx, conn, http.MethodGet, "/users/me/history?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: gmail: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	// Gmail responds with 404 when startHistoryId is too old; treat
	// as a re-sync signal but keep the cursor so caller decides.
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("gmail: history status=%d", resp.StatusCode)
	}
	var body struct {
		History []struct {
			ID            string `json:"id"`
			MessagesAdded []struct {
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			} `json:"messagesAdded"`
			MessagesDeleted []struct {
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			} `json:"messagesDeleted"`
		} `json:"history"`
		HistoryID string `json:"historyId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("gmail: decode history: %w", err)
	}
	var changes []connector.DocumentChange
	for _, h := range body.History {
		for _, ma := range h.MessagesAdded {
			changes = append(changes, connector.DocumentChange{
				Kind: connector.ChangeUpserted,
				Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: ma.Message.ID, ETag: h.ID},
			})
		}
		for _, md := range h.MessagesDeleted {
			changes = append(changes, connector.DocumentChange{
				Kind: connector.ChangeDeleted,
				Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: md.Message.ID, ETag: h.ID},
			})
		}
	}
	newCursor := body.HistoryID
	if newCursor == "" {
		newCursor = cursor
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper.
func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("gmail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gmail: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Gmail connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
