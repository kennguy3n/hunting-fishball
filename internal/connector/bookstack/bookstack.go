// Package bookstack implements the BookStack SourceConnector +
// DeltaSyncer against the BookStack REST API at
// https://<host>/api. Authentication is a Token-ID + Token-Secret
// pair carried via the BookStack-specific `Authorization: Token
// <id>:<secret>` header.
//
// Credentials must be a JSON blob:
//
//	{
//	  "base_url":     "https://wiki.example.com", // required
//	  "token_id":     "...",                      // required
//	  "token_secret": "..."                       // required
//	}
//
// Delta is implemented via the BookStack pages endpoint filtered
// by `updated_at` with a sort order of `+updated_at` so the
// caller can stop iterating at the cursor.
package bookstack

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

const (
	// Name is the registry-visible connector name.
	Name = "bookstack"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	BaseURL     string `json:"base_url"`
	TokenID     string `json:"token_id"`
	TokenSecret string `json:"token_secret"`
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

// WithBaseURL pins the BookStack base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID    string
	sourceID    string
	baseURL     string
	tokenID     string
	tokenSecret string
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
	if o.baseURL == "" && creds.BaseURL == "" {
		return fmt.Errorf("%w: base_url required", connector.ErrInvalidConfig)
	}
	if creds.TokenID == "" || creds.TokenSecret == "" {
		return fmt.Errorf("%w: token_id and token_secret required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /api/users/me-style ping (/api/books?count=1).
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := o.baseURL
	if base == "" {
		base = creds.BaseURL
	}
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		baseURL:     base,
		tokenID:     creds.TokenID,
		tokenSecret: creds.TokenSecret,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/books?count=1", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: bookstack: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bookstack: books status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns "pages" as the only namespace.
// BookStack books / chapters are flattened — every page is
// surfaced individually with its enclosing book/chapter recorded
// in the document metadata.
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "pages", Name: "Pages", Kind: "wiki"}}, nil
}

type pageEntry struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	BookID    int    `json:"book_id"`
	ChapterID int    `json:"chapter_id"`
	UpdatedAt string `json:"updated_at"`
	CreatedAt string `json:"created_at"`
	HTML      string `json:"html,omitempty"`
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
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/api/pages?sort=-updated_at", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: bookstack: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("bookstack: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Data []pageEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("bookstack: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, p := range body.Data {
		ref := connector.DocumentRef{NamespaceID: "pages", ID: fmt.Sprintf("%d", p.ID)}
		if t, perr := time.Parse(time.RFC3339, p.UpdatedAt); perr == nil {
			ref.UpdatedAt = t.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments enumerates BookStack pages.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bookstack: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads page metadata + HTML body.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bookstack: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/pages/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: bookstack: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bookstack: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var body pageEntry
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("bookstack: decode page: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, body.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, body.UpdatedAt)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "pages", ID: fmt.Sprintf("%d", body.ID), UpdatedAt: updated},
		MIMEType:  "text/html",
		Title:     body.Name,
		Size:      int64(len(body.HTML)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.HTML)),
		Metadata: map[string]string{
			"book_id":    fmt.Sprintf("%d", body.BookID),
			"chapter_id": fmt.Sprintf("%d", body.ChapterID),
		},
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /api/pages?sort=-updated_at and stops at the
// cursor. The empty-cursor bootstrap returns the most-recently
// updated page's `updated_at` without backfilling.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("bookstack: bad connection type")
	}
	if cursor == "" {
		resp, err := o.do(ctx, conn, http.MethodGet, "/api/pages?sort=-updated_at&count=1", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: bookstack: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("bookstack: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Data []pageEntry `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("bookstack: decode bootstrap: %w", err)
		}
		if len(body.Data) == 0 {
			return nil, time.Now().UTC().Format(time.RFC3339), nil
		}

		return nil, body.Data[0].UpdatedAt, nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("bookstack: bad cursor %q: %w", cursor, err)
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/pages?sort=-updated_at", nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: bookstack: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("bookstack: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Data []pageEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("bookstack: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, p := range body.Data {
		t, perr := time.Parse(time.RFC3339, p.UpdatedAt)
		if perr != nil {
			continue
		}
		t = t.UTC()
		if !t.After(cursorTime) {
			break
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: fmt.Sprintf("%d", p.ID), UpdatedAt: t},
		})
		if t.After(newest) {
			newest = t
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("bookstack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+conn.tokenID+":"+conn.tokenSecret)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bookstack: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
