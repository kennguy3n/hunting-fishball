// Package clickup implements the ClickUp SourceConnector +
// DeltaSyncer against the ClickUp v2 REST API at
// https://api.clickup.com/api/v2. Authentication uses an OAuth
// access token or personal API key (passed as `Authorization`
// without a scheme prefix). We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_key":  "pk_...",      // required (PAT or OAuth)
//	  "team_id":  "1234"           // required
//	}
package clickup

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
	Name = "clickup"

	defaultBaseURL  = "https://api.clickup.com/api/v2"
	defaultPageSize = 100
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIKey string `json:"api_key"`
	TeamID string `json:"team_id"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(s *Connector) { s.httpClient = c } }

// WithBaseURL overrides the ClickUp API base URL — used by tests.
func WithBaseURL(u string) Option { return func(s *Connector) { s.baseURL = u } }

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
	apiKey   string
	teamID   string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (c *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.APIKey == "" {
		return fmt.Errorf("%w: api_key required", connector.ErrInvalidConfig)
	}
	if creds.TeamID == "" {
		return fmt.Errorf("%w: team_id required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /user as the auth check.
func (c *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := c.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, apiKey: creds.APIKey, teamID: creds.TeamID}
	resp, err := c.do(ctx, conn, http.MethodGet, "/user", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickup: /user status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the team's lists. ClickUp's hierarchy is
// team → space → folder → list; we surface lists as namespaces.
func (c *Connector) ListNamespaces(ctx context.Context, cc connector.Connection) ([]connector.Namespace, error) {
	conn, ok := cc.(*connection)
	if !ok {
		return nil, errors.New("clickup: bad connection type")
	}
	spaces, err := c.fetchSpaces(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := []connector.Namespace{}
	for _, sp := range spaces {
		lists, err := c.fetchLists(ctx, conn, sp.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range lists {
			out = append(out, connector.Namespace{
				ID: l.ID, Name: l.Name, Kind: "list",
				Metadata: map[string]string{"space_id": sp.ID, "space_name": sp.Name},
			})
		}
	}

	return out, nil
}

type spaceLite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Connector) fetchSpaces(ctx context.Context, conn *connection) ([]spaceLite, error) {
	resp, err := c.do(ctx, conn, http.MethodGet, "/team/"+conn.teamID+"/space?archived=false", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickup: spaces status=%d", resp.StatusCode)
	}
	var body struct {
		Spaces []spaceLite `json:"spaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("clickup: decode spaces: %w", err)
	}

	return body.Spaces, nil
}

func (c *Connector) fetchLists(ctx context.Context, conn *connection, spaceID string) ([]spaceLite, error) {
	resp, err := c.do(ctx, conn, http.MethodGet, "/space/"+spaceID+"/list?archived=false", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickup: lists status=%d", resp.StatusCode)
	}
	var body struct {
		Lists []spaceLite `json:"lists"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("clickup: decode lists: %w", err)
	}

	return body.Lists, nil
}

// taskIterator paginates tasks in a list.
type taskIterator struct {
	c       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  int
	err     error
	done    bool
}

func (it *taskIterator) Next(ctx context.Context) bool {
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

func (it *taskIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *taskIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *taskIterator) Close() error { return nil }

func (it *taskIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("archived", "false")
	q.Set("page", strconv.Itoa(it.cursor))
	resp, err := it.c.do(ctx, it.conn, http.MethodGet, "/list/"+it.ns.ID+"/task?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("clickup: tasks status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Tasks []struct {
			ID          string `json:"id"`
			DateUpdated string `json:"date_updated"`
		} `json:"tasks"`
		LastPage bool `json:"last_page"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("clickup: decode tasks: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, t := range body.Tasks {
		updatedMs, _ := strconv.ParseInt(t.DateUpdated, 10, 64)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          t.ID,
			ETag:        t.DateUpdated,
			UpdatedAt:   time.UnixMilli(updatedMs),
		})
	}
	if body.LastPage || len(body.Tasks) == 0 || len(body.Tasks) < defaultPageSize {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor++
	}

	return true
}

// ListDocuments enumerates tasks in the list namespace.
func (c *Connector) ListDocuments(_ context.Context, cc connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := cc.(*connection)
	if !ok {
		return nil, errors.New("clickup: bad connection type")
	}

	return &taskIterator{c: c, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single task by ID.
func (c *Connector) FetchDocument(ctx context.Context, cc connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := cc.(*connection)
	if !ok {
		return nil, errors.New("clickup: bad connection type")
	}
	resp, err := c.do(ctx, conn, http.MethodGet, "/task/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clickup: get task status=%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("clickup: read body: %w", err)
	}
	var body struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		DateCreated string `json:"date_created"`
		DateUpdated string `json:"date_updated"`
		Creator     struct {
			Username string `json:"username"`
		} `json:"creator"`
	}
	_ = json.Unmarshal(raw, &body)
	created, _ := strconv.ParseInt(body.DateCreated, 10, 64)
	updated, _ := strconv.ParseInt(body.DateUpdated, 10, 64)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     body.Name,
		Author:    body.Creator.Username,
		Size:      int64(len(raw)),
		CreatedAt: time.UnixMilli(created),
		UpdatedAt: time.UnixMilli(updated),
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"list_id": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — ClickUp pushes change events through
// its webhooks system (HandleWebhook in a later round).
func (c *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (c *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses /list/{listId}/task?date_updated_gt= to fetch
// tasks updated since the cursor (ms epoch). Empty cursor on the
// first call bootstraps from the single most-recently-updated
// task (order_by=updated, reverse=true, page=0 with 1 result
// effectively) so the next call only sees changes that happened
// after we started — not a 100-task backfill from the start of
// the list.
func (c *Connector) DeltaSync(ctx context.Context, cc connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := cc.(*connection)
	if !ok {
		return nil, "", errors.New("clickup: bad connection type")
	}
	if cursor == "" {
		q := url.Values{}
		q.Set("archived", "false")
		q.Set("page", "0")
		q.Set("order_by", "updated")
		q.Set("reverse", "true")
		resp, err := c.do(ctx, conn, http.MethodGet, "/list/"+ns.ID+"/task?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("clickup: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Tasks []struct {
				DateUpdated string `json:"date_updated"`
			} `json:"tasks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("clickup: decode bootstrap: %w", err)
		}
		var newest int64
		for _, t := range body.Tasks {
			ms, _ := strconv.ParseInt(t.DateUpdated, 10, 64)
			if ms > newest {
				newest = ms
			}
		}
		if newest == 0 {
			newest = time.Now().UnixMilli()
		}

		return nil, strconv.FormatInt(newest, 10), nil
	}
	q := url.Values{}
	q.Set("archived", "false")
	q.Set("date_updated_gt", cursor)
	q.Set("order_by", "updated")
	resp, err := c.do(ctx, conn, http.MethodGet, "/list/"+ns.ID+"/task?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: clickup: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("clickup: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Tasks []struct {
			ID          string `json:"id"`
			DateUpdated string `json:"date_updated"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("clickup: decode delta: %w", err)
	}
	newCursor, _ := strconv.ParseInt(cursor, 10, 64)
	changes := make([]connector.DocumentChange, 0, len(body.Tasks))
	for _, t := range body.Tasks {
		ms, _ := strconv.ParseInt(t.DateUpdated, 10, 64)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          t.ID,
				ETag:        t.DateUpdated,
				UpdatedAt:   time.UnixMilli(ms),
			},
		})
		if ms > newCursor {
			newCursor = ms
		}
	}

	return changes, strconv.FormatInt(newCursor, 10), nil
}

// do is the shared HTTP helper.
func (c *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(c.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("clickup: build request: %w", err)
	}
	// ClickUp expects the API key directly in the Authorization
	// header (no Bearer prefix).
	req.Header.Set("Authorization", conn.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickup: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the ClickUp connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
