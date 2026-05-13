// Package trello implements the Trello SourceConnector +
// DeltaSyncer against the Trello REST API at
// https://api.trello.com/1. Authentication uses Trello's classic
// API-key + token pair carried as `key=...&token=...` query
// parameters on every request.
//
// Credentials must be a JSON blob:
//
//	{
//	 "api_key":  "...",   // required
//	 "token":    "...",   // required
//	 "board_id": "..."    // required (Trello board id)
//	}
//
// The connector exposes the configured board as the single
// namespace. Delta uses `/1/boards/{id}/actions?since=<ISO8601>`
// with a `before` cursor — Trello's actions endpoint returns
// updates ordered newest-first so we sort on the way out.
package trello

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

// Name is the registry-visible connector name.
const Name = "trello"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIKey  string `json:"api_key"`
	Token   string `json:"token"`
	BoardID string `json:"board_id"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(o *Connector) { o.httpClient = c } }

// WithBaseURL pins the Trello base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.trello.com"}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID string
	sourceID string
	baseURL  string
	apiKey   string
	token    string
	boardID  string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (o *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if creds.Token == "" {
		return fmt.Errorf("%w: token required", connector.ErrInvalidConfig)
	}
	if creds.BoardID == "" {
		return fmt.Errorf("%w: board_id required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect verifies the board is reachable.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{
		tenantID: cfg.TenantID,
		sourceID: cfg.SourceID,
		baseURL:  o.baseURL,
		apiKey:   creds.APIKey,
		token:    creds.Token,
		boardID:  creds.BoardID,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/1/boards/"+url.PathEscape(conn.boardID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: trello: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trello: board status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the configured board id as a single namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("trello: bad connection type")
	}

	return []connector.Namespace{{ID: conn.boardID, Name: conn.boardID, Kind: "board"}}, nil
}

type cardEntry struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Desc             string `json:"desc"`
	DateLastActivity string `json:"dateLastActivity"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
}

func (it *docIterator) Next(ctx context.Context) bool {
	if it.err != nil || it.done {
		return false
	}
	if it.idx >= len(it.page) {
		if !it.fetch(ctx) {
			return false
		}
	}
	if it.idx < len(it.page) {
		it.idx++

		return true
	}

	return false
}

func (it *docIterator) Doc() connector.DocumentRef {
	if it.idx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.idx-1]
}

func (it *docIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *docIterator) Close() error { return nil }

func (it *docIterator) fetch(ctx context.Context) bool {
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/1/boards/"+url.PathEscape(it.conn.boardID)+"/cards", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: trello: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("trello: list status=%d", resp.StatusCode)

		return false
	}
	var cards []cardEntry
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		it.err = fmt.Errorf("trello: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, c := range cards {
		ref := connector.DocumentRef{NamespaceID: it.conn.boardID, ID: c.ID}
		if ts, perr := time.Parse(time.RFC3339, c.DateLastActivity); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments enumerates Trello cards on the configured board.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("trello: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a single Trello card.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("trello: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/1/cards/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: trello: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trello: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var card cardEntry
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return nil, fmt.Errorf("trello: decode card: %w", err)
	}
	updated, _ := time.Parse(time.RFC3339, card.DateLastActivity)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: conn.boardID, ID: card.ID, UpdatedAt: updated},
		MIMEType:  "text/markdown",
		Title:     card.Name,
		Size:      int64(len(card.Desc)),
		CreatedAt: updated,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(card.Desc)),
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

type actionEntry struct {
	ID   string `json:"id"`
	Date string `json:"date"`
	Data struct {
		Card struct {
			ID string `json:"id"`
		} `json:"card"`
	} `json:"data"`
}

// DeltaSync uses the board actions endpoint with `since` filter.
// Empty cursor returns "now" (bootstrap contract).
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("trello: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("trello: bad cursor %q: %w", cursor, err)
	}
	q := url.Values{}
	q.Set("since", cursor)
	q.Set("filter", "updateCard,createCard")
	q.Set("limit", "100")
	resp, err := o.do(ctx, conn, http.MethodGet, "/1/boards/"+url.PathEscape(conn.boardID)+"/actions?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: trello: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("trello: delta status=%d", resp.StatusCode)
	}
	var actions []actionEntry
	if err := json.NewDecoder(resp.Body).Decode(&actions); err != nil {
		return nil, "", fmt.Errorf("trello: decode delta: %w", err)
	}
	newest := cursorTime
	seen := make(map[string]struct{})
	var changes []connector.DocumentChange
	for _, a := range actions {
		ts, _ := time.Parse(time.RFC3339, a.Date)
		ts = ts.UTC()
		if a.Data.Card.ID == "" {
			continue
		}
		if _, dup := seen[a.Data.Card.ID]; dup {
			continue
		}
		seen[a.Data.Card.ID] = struct{}{}
		ref := connector.DocumentRef{NamespaceID: ns.ID, ID: a.Data.Card.ID, UpdatedAt: ts}
		changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		if ts.After(newest) {
			newest = ts
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	target := strings.TrimRight(conn.baseURL, "/") + path + sep + "key=" + url.QueryEscape(conn.apiKey) + "&token=" + url.QueryEscape(conn.token)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("trello: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trello: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
