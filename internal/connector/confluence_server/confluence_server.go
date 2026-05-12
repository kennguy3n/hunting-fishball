// Package confluenceserver implements the Confluence
// Server/Data Center SourceConnector + DeltaSyncer against
// the REST API at https://<host>/rest/api. Authentication uses
// either a Personal Access Token (Bearer) or basic auth.
// We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "base_url": "https://wiki.acme.com",  // required
//	  "pat":      "...",                    // optional (preferred)
//	  "username": "alice",                  // optional (basic auth)
//	  "password": "..."                     // optional
//	}
//
// One of pat OR (username+password) must be set.
package confluenceserver

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
	Name = "confluence_server"

	defaultPageSize = 50
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	BaseURL  string `json:"base_url"`
	PAT      string `json:"pat,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
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

// WithBaseURL overrides the Confluence base URL — used by tests.
func WithBaseURL(u string) Option { return func(s *Connector) { s.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	s := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

type connection struct {
	tenantID string
	sourceID string
	baseURL  string
	pat      string
	username string
	password string
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
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if s.baseURL == "" && creds.BaseURL == "" {
		return fmt.Errorf("%w: base_url required", connector.ErrInvalidConfig)
	}
	if creds.PAT == "" && (creds.Username == "" || creds.Password == "") {
		return fmt.Errorf("%w: pat or username+password required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /rest/api/user/current as the auth check.
func (s *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := s.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	base := s.baseURL
	if base == "" {
		base = creds.BaseURL
	}
	conn := &connection{
		tenantID: cfg.TenantID,
		sourceID: cfg.SourceID,
		baseURL:  base,
		pat:      creds.PAT,
		username: creds.Username,
		password: creds.Password,
	}
	resp, err := s.do(ctx, conn, http.MethodGet, "/rest/api/user/current", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence_server: /user/current status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns Confluence spaces as namespaces.
func (s *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence_server: bad connection type")
	}
	var out []connector.Namespace
	start := 0
	for {
		q := url.Values{}
		q.Set("start", strconv.Itoa(start))
		q.Set("limit", strconv.Itoa(defaultPageSize))
		resp, err := s.do(ctx, conn, http.MethodGet, "/rest/api/space?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("confluence_server: list spaces status=%d", resp.StatusCode)
		}
		var body struct {
			Results []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"results"`
			Size  int `json:"size"`
			Limit int `json:"limit"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("confluence_server: decode spaces: %w", err)
		}
		_ = resp.Body.Close()
		for _, sp := range body.Results {
			out = append(out, connector.Namespace{ID: sp.Key, Name: sp.Name, Kind: "space"})
		}
		if body.Size < body.Limit || body.Limit == 0 {
			break
		}
		start += body.Size
	}

	return out, nil
}

// pageIterator paginates pages in a space.
type pageIterator struct {
	s       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	start   int
	first   bool
	err     error
	done    bool
}

func (it *pageIterator) Next(ctx context.Context) bool {
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

func (it *pageIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *pageIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *pageIterator) Close() error { return nil }

func (it *pageIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("spaceKey", it.ns.ID)
	q.Set("type", "page")
	q.Set("start", strconv.Itoa(it.start))
	q.Set("limit", strconv.Itoa(defaultPageSize))
	q.Set("expand", "version")
	resp, err := it.s.do(ctx, it.conn, http.MethodGet, "/rest/api/content?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("confluence_server: list pages status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Results []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Version struct {
				When string `json:"when"`
			} `json:"version"`
		} `json:"results"`
		Size  int `json:"size"`
		Limit int `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("confluence_server: decode pages: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, p := range body.Results {
		updated, _ := time.Parse(time.RFC3339, p.Version.When)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          p.ID,
			ETag:        p.Version.When,
			UpdatedAt:   updated,
		})
	}
	if body.Size < body.Limit {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.start += body.Size
	}
	it.first = true

	return true
}

// ListDocuments enumerates pages in the space namespace.
func (s *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence_server: bad connection type")
	}

	return &pageIterator{s: s, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single page with body.storage.
func (s *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("confluence_server: bad connection type")
	}
	q := url.Values{}
	q.Set("expand", "body.storage,version")
	resp, err := s.do(ctx, conn, http.MethodGet, "/rest/api/content/"+ref.ID+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("confluence_server: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("confluence_server: read body: %w", err)
	}
	var body struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Version struct {
			By struct {
				DisplayName string `json:"displayName"`
			} `json:"by"`
			When string `json:"when"`
		} `json:"version"`
		Body struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
	}
	_ = json.Unmarshal(raw, &body)
	updated, _ := time.Parse(time.RFC3339, body.Version.When)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/html",
		Title:     body.Title,
		Author:    body.Version.By.DisplayName,
		Size:      int64(len(body.Body.Storage.Value)),
		CreatedAt: updated,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.Body.Storage.Value)),
		Metadata:  map[string]string{"version_when": body.Version.When},
	}, nil
}

// Subscribe is unsupported — Confluence Server supports webhooks
// (HandleWebhook in a later round).
func (s *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (s *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses CQL `space=<space> AND lastModified > "<cursor>"`
// via the /rest/api/content/search endpoint. Empty cursor on the
// first call returns the newest page's lastModified without
// backfilling history.
func (s *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("confluence_server: bad connection type")
	}
	if cursor == "" {
		// Bootstrap: most-recently-modified page in the space.
		q := url.Values{}
		q.Set("cql", fmt.Sprintf(`space = "%s" AND type = "page" ORDER BY lastModified DESC`, ns.ID))
		q.Set("limit", "1")
		q.Set("expand", "version")
		resp, err := s.do(ctx, conn, http.MethodGet, "/rest/api/content/search?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("confluence_server: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Results []struct {
				Version struct {
					When string `json:"when"`
				} `json:"version"`
			} `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("confluence_server: decode bootstrap: %w", err)
		}
		newCursor := time.Now().UTC().Format(time.RFC3339)
		if len(body.Results) > 0 && body.Results[0].Version.When != "" {
			newCursor = body.Results[0].Version.When
		}

		return nil, newCursor, nil
	}
	q := url.Values{}
	cursorDate := cursor
	if t, err := time.Parse(time.RFC3339, cursor); err == nil {
		cursorDate = t.UTC().Format("2006-01-02 15:04")
	}
	q.Set("cql", fmt.Sprintf(`space = "%s" AND type = "page" AND lastModified > "%s" ORDER BY lastModified ASC`, ns.ID, cursorDate))
	q.Set("limit", strconv.Itoa(defaultPageSize))
	q.Set("expand", "version")
	resp, err := s.do(ctx, conn, http.MethodGet, "/rest/api/content/search?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: confluence_server: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("confluence_server: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Results []struct {
			ID      string `json:"id"`
			Version struct {
				When string `json:"when"`
			} `json:"version"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("confluence_server: decode delta: %w", err)
	}
	newCursor := cursor
	changes := make([]connector.DocumentChange, 0, len(body.Results))
	for _, p := range body.Results {
		updated, _ := time.Parse(time.RFC3339, p.Version.When)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          p.ID,
				ETag:        p.Version.When,
				UpdatedAt:   updated,
			},
		})
		if p.Version.When > newCursor {
			newCursor = p.Version.When
		}
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper.
func (s *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("confluence_server: build request: %w", err)
	}
	if conn.pat != "" {
		req.Header.Set("Authorization", "Bearer "+conn.pat)
	} else if conn.username != "" {
		req.SetBasicAuth(conn.username, conn.password)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence_server: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Confluence Server connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
