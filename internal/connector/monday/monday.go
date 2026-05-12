// Package monday implements the Monday.com SourceConnector +
// DeltaSyncer against the Monday GraphQL API v2 at
// https://api.monday.com/v2. Authentication uses a personal API
// token (passed as `Authorization` without scheme prefix). We
// use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "api_token": "eyJ..." }   // required
package monday

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
	// Name is the registry-visible connector name.
	Name = "monday"

	defaultBaseURL = "https://api.monday.com/v2"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIToken string `json:"api_token"`
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

// WithBaseURL overrides the Monday API base URL — used by tests.
func WithBaseURL(u string) Option { return func(m *Connector) { m.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	m := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
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
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if creds.APIToken == "" {
		return fmt.Errorf("%w: api_token required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect runs a `query { me { id } }` GraphQL probe as the auth check.
func (m *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := m.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.APIToken}
	var resp struct {
		Data struct {
			Me struct {
				ID string `json:"id"`
			} `json:"me"`
		} `json:"data"`
	}
	if err := m.gql(ctx, conn, `query{me{id}}`, nil, &resp); err != nil {
		return nil, err
	}

	return conn, nil
}

// ListNamespaces returns the user's boards as namespaces.
func (m *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("monday: bad connection type")
	}
	var resp struct {
		Data struct {
			Boards []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"boards"`
		} `json:"data"`
	}
	if err := m.gql(ctx, conn, `query{boards(limit:500){id name}}`, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]connector.Namespace, 0, len(resp.Data.Boards))
	for _, b := range resp.Data.Boards {
		out = append(out, connector.Namespace{ID: b.ID, Name: b.Name, Kind: "board"})
	}

	return out, nil
}

// itemIterator paginates items in a board using items_page cursor.
type itemIterator struct {
	m       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
	first   bool
	err     error
	done    bool
}

func (it *itemIterator) Next(ctx context.Context) bool {
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

func (it *itemIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *itemIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *itemIterator) Close() error { return nil }

func (it *itemIterator) fetchPage(ctx context.Context) bool {
	var query string
	vars := map[string]any{}
	if !it.first {
		it.first = true
		query = `query($id:[ID!]){boards(ids:$id){items_page(limit:100){cursor items{id updated_at}}}}`
		vars["id"] = []string{it.ns.ID}
	} else {
		query = `query($c:String!){next_items_page(limit:100,cursor:$c){cursor items{id updated_at}}}`
		vars["c"] = it.cursor
	}
	var resp struct {
		Data struct {
			Boards []struct {
				ItemsPage *itemsPage `json:"items_page"`
			} `json:"boards"`
			NextItemsPage *itemsPage `json:"next_items_page"`
		} `json:"data"`
	}
	if err := it.m.gql(ctx, it.conn, query, vars, &resp); err != nil {
		it.err = err

		return false
	}
	var page *itemsPage
	if resp.Data.NextItemsPage != nil {
		page = resp.Data.NextItemsPage
	} else if len(resp.Data.Boards) > 0 && resp.Data.Boards[0].ItemsPage != nil {
		page = resp.Data.Boards[0].ItemsPage
	}
	if page == nil {
		it.done = true

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, item := range page.Items {
		updated, _ := time.Parse(time.RFC3339, item.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          item.ID,
			ETag:        item.UpdatedAt,
			UpdatedAt:   updated,
		})
	}
	if page.Cursor == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = page.Cursor
	}

	return true
}

type itemsPage struct {
	Cursor string `json:"cursor"`
	Items  []struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updated_at"`
	} `json:"items"`
}

// ListDocuments enumerates items in the board namespace.
func (m *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("monday: bad connection type")
	}

	return &itemIterator{m: m, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single item by ID.
func (m *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("monday: bad connection type")
	}
	var resp struct {
		Data struct {
			Items []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				CreatedAt string `json:"created_at"`
				UpdatedAt string `json:"updated_at"`
				Creator   struct {
					Name string `json:"name"`
				} `json:"creator"`
			} `json:"items"`
		} `json:"data"`
	}
	q := `query($id:[ID!]!){items(ids:$id){id name created_at updated_at creator{name}}}`
	if err := m.gql(ctx, conn, q, map[string]any{"id": []string{ref.ID}}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data.Items) == 0 {
		return nil, fmt.Errorf("monday: item not found: %s", ref.ID)
	}
	it := resp.Data.Items[0]
	raw, _ := json.Marshal(it)
	created, _ := time.Parse(time.RFC3339, it.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, it.UpdatedAt)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     it.Name,
		Author:    it.Creator.Name,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"board_id": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Monday pushes change events via
// webhooks (HandleWebhook in a later round).
func (m *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (m *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses items_page_by_column_values to fetch items whose
// updated_at column is greater than the cursor (RFC3339).
// Bootstrap (empty cursor) returns the newest item's updated_at
// without backfilling, matching the Round-15 contract.
func (m *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("monday: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: fetch 1 most-recently-updated item from the
		// board to capture "now". With Monday's GraphQL we order by
		// updated_at desc via items_page with a query_params order.
		q := `query($id:[ID!]){boards(ids:$id){items_page(limit:1,query_params:{order_by:[{column_id:"__last_updated__",direction:desc}]}){items{updated_at}}}}`
		var resp struct {
			Data struct {
				Boards []struct {
					ItemsPage struct {
						Items []struct {
							UpdatedAt string `json:"updated_at"`
						} `json:"items"`
					} `json:"items_page"`
				} `json:"boards"`
			} `json:"data"`
		}
		if err := m.gql(ctx, conn, q, map[string]any{"id": []string{ns.ID}}, &resp); err != nil {
			return nil, "", err
		}
		newCursor := time.Now().UTC().Format(time.RFC3339)
		if len(resp.Data.Boards) > 0 && len(resp.Data.Boards[0].ItemsPage.Items) > 0 {
			newCursor = resp.Data.Boards[0].ItemsPage.Items[0].UpdatedAt
		}

		return nil, newCursor, nil
	}
	q := `query($id:[ID!],$ts:CompareValue!){boards(ids:$id){items_page(limit:100,query_params:{rules:[{column_id:"__last_updated__",compare_value:$ts,operator:greater_than}],order_by:[{column_id:"__last_updated__",direction:asc}]}){cursor items{id updated_at}}}}`
	var resp struct {
		Data struct {
			Boards []struct {
				ItemsPage itemsPage `json:"items_page"`
			} `json:"boards"`
		} `json:"data"`
	}
	if err := m.gql(ctx, conn, q, map[string]any{"id": []string{ns.ID}, "ts": cursor}, &resp); err != nil {
		return nil, "", err
	}
	newCursor := cursor
	var changes []connector.DocumentChange
	if len(resp.Data.Boards) > 0 {
		for _, item := range resp.Data.Boards[0].ItemsPage.Items {
			updated, _ := time.Parse(time.RFC3339, item.UpdatedAt)
			changes = append(changes, connector.DocumentChange{
				Kind: connector.ChangeUpserted,
				Ref: connector.DocumentRef{
					NamespaceID: ns.ID,
					ID:          item.ID,
					ETag:        item.UpdatedAt,
					UpdatedAt:   updated,
				},
			})
			if item.UpdatedAt > newCursor {
				newCursor = item.UpdatedAt
			}
		}
	}

	return changes, newCursor, nil
}

// gql sends a GraphQL request, decodes the response, and surfaces
// HTTP 429 and Monday's complexity-quota responses as
// connector.ErrRateLimited.
func (m *Connector) gql(ctx context.Context, conn *connection, query string, vars map[string]any, out any) error {
	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	enc, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("monday: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.baseURL, "/"), bytes.NewReader(enc))
	if err != nil {
		return fmt.Errorf("monday: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("API-Version", "2024-01")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("monday: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// HTTP 429 → wrap as ErrRateLimited.
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: monday: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("monday: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Monday also returns 200 with errors, but a non-200 is an
		// outright failure.
		return fmt.Errorf("monday: status=%d body=%s", resp.StatusCode, string(raw))
	}
	// Monday signals complexity-budget exhaustion via a 200-OK
	// `errors` envelope with status_code 429 — wrap as
	// ErrRateLimited so the adaptive limiter can react.
	var probe struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code       string `json:"code"`
				StatusCode int    `json:"status_code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &probe)
	for _, e := range probe.Errors {
		if e.Extensions.StatusCode == http.StatusTooManyRequests ||
			strings.EqualFold(e.Extensions.Code, "ComplexityException") ||
			strings.EqualFold(e.Extensions.Code, "RateLimitExceeded") ||
			strings.Contains(strings.ToLower(e.Message), "complexity budget") {
			return fmt.Errorf("%w: monday: %s", connector.ErrRateLimited, e.Message)
		}
	}
	if len(probe.Errors) > 0 {
		return fmt.Errorf("monday: graphql errors: %s", probe.Errors[0].Message)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("monday: decode: %w", err)
	}

	return nil
}

// Register registers the Monday connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
