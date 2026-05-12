// Package asana implements the Asana SourceConnector +
// DeltaSyncer. Asana exposes REST endpoints under
// https://app.asana.com/api/1.0 authenticated with a bearer
// token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "access_token": "..." }   // required
package asana

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
	Name = "asana"

	defaultBaseURL = "https://app.asana.com/api/1.0"
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
func WithHTTPClient(c *http.Client) Option { return func(a *Connector) { a.httpClient = c } }

// WithBaseURL overrides the Asana API base URL — used by tests.
func WithBaseURL(u string) Option { return func(a *Connector) { a.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	a := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(a)
	}

	return a
}

type connection struct {
	tenantID string
	sourceID string
	token    string
	userGID  string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (a *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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

// Connect calls /users/me as a cheap auth check.
func (a *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := a.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken}

	resp, err := a.do(ctx, conn, http.MethodGet, "/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asana: users/me status=%d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			GID string `json:"gid"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("asana: decode users/me: %w", err)
	}
	conn.userGID = body.Data.GID

	return conn, nil
}

// ListNamespaces returns projects across the user's workspaces.
func (a *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("asana: bad connection type")
	}
	// First: enumerate workspaces.
	resp, err := a.do(ctx, conn, http.MethodGet, "/workspaces", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("asana: workspaces status=%d", resp.StatusCode)
	}
	var ws struct {
		Data []struct {
			GID  string `json:"gid"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("asana: decode workspaces: %w", err)
	}
	_ = resp.Body.Close()

	out := []connector.Namespace{}
	for _, w := range ws.Data {
		// Fetch projects for each workspace; paginate via offset.
		offset := ""
		for {
			q := url.Values{}
			q.Set("workspace", w.GID)
			q.Set("limit", "100")
			if offset != "" {
				q.Set("offset", offset)
			}
			r, err := a.do(ctx, conn, http.MethodGet, "/projects?"+q.Encode(), nil)
			if err != nil {
				return nil, err
			}
			if r.StatusCode != http.StatusOK {
				_ = r.Body.Close()

				return nil, fmt.Errorf("asana: projects status=%d", r.StatusCode)
			}
			var pr struct {
				Data []struct {
					GID  string `json:"gid"`
					Name string `json:"name"`
				} `json:"data"`
				NextPage struct {
					Offset string `json:"offset"`
				} `json:"next_page"`
			}
			if err := json.NewDecoder(r.Body).Decode(&pr); err != nil {
				_ = r.Body.Close()

				return nil, fmt.Errorf("asana: decode projects: %w", err)
			}
			_ = r.Body.Close()
			for _, p := range pr.Data {
				out = append(out, connector.Namespace{
					ID:       p.GID,
					Name:     w.Name + "/" + p.Name,
					Kind:     "project",
					Metadata: map[string]string{"workspace_gid": w.GID, "project_gid": p.GID},
				})
			}
			if pr.NextPage.Offset == "" {
				break
			}
			offset = pr.NextPage.Offset
		}
	}

	return out, nil
}

// taskIterator paginates over /projects/{gid}/tasks.
type taskIterator struct {
	a       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
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
	q.Set("limit", strconv.Itoa(pageOr(it.opts.PageSize, 100)))
	q.Set("opt_fields", "gid,name,modified_at,created_at")
	if it.cursor != "" {
		q.Set("offset", it.cursor)
	} else if it.opts.PageToken != "" {
		q.Set("offset", it.opts.PageToken)
	}
	if !it.opts.Since.IsZero() {
		q.Set("modified_since", it.opts.Since.UTC().Format(time.RFC3339))
	}

	resp, err := it.a.do(ctx, it.conn, http.MethodGet, "/projects/"+it.ns.ID+"/tasks?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: asana: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("asana: tasks status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Data []struct {
			GID        string `json:"gid"`
			Name       string `json:"name"`
			ModifiedAt string `json:"modified_at"`
		} `json:"data"`
		NextPage struct {
			Offset string `json:"offset"`
		} `json:"next_page"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("asana: decode tasks: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, t := range body.Data {
		updated, _ := time.Parse(time.RFC3339, t.ModifiedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          t.GID,
			ETag:        t.ModifiedAt,
			UpdatedAt:   updated,
		})
	}
	if body.NextPage.Offset == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.NextPage.Offset
	}

	return true
}

// ListDocuments enumerates tasks in a project.
func (a *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("asana: bad connection type")
	}

	return &taskIterator{a: a, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single task by GID.
func (a *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("asana: bad connection type")
	}
	q := url.Values{}
	q.Set("opt_fields", "gid,name,notes,created_at,modified_at,assignee")

	resp, err := a.do(ctx, conn, http.MethodGet, "/tasks/"+ref.ID+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asana: tasks/%s status=%d", ref.ID, resp.StatusCode)
	}
	var body struct {
		Data struct {
			GID        string `json:"gid"`
			Name       string `json:"name"`
			Notes      string `json:"notes"`
			CreatedAt  string `json:"created_at"`
			ModifiedAt string `json:"modified_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("asana: decode task: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, body.Data.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, body.Data.ModifiedAt)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/plain",
		Title:     body.Data.Name,
		Size:      int64(len(body.Data.Notes) + len(body.Data.Name)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.Data.Notes)),
	}, nil
}

// Subscribe is unsupported — Asana ships its push events through
// the webhook plane (out of scope for this connector).
func (a *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (a *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses Asana's workspace search endpoint
// (/workspaces/{wgid}/tasks/search) for BOTH bootstrap and
// steady-state to guarantee monotonic cursor advancement.
//
// Asana's /projects/{gid}/tasks list endpoint returns tasks in
// creation / task-list-rank order — it does NOT support
// sort_by=modified_at. Server-side filtering with modified_since
// works, but the page-of-100 returned is creation-ordered, so
// taking max(modified_at) over the first 100 and advancing the
// cursor skips any task whose modified_at falls in the active
// change window but whose creation rank places it beyond task
// #100. That violates the DeltaSyncer "no skipped changes"
// contract.
//
// The workspace search endpoint /workspaces/{wgid}/tasks/search
// supports server-side sort_by=modified_at, so each page is
// modification-ordered:
//
//   - Bootstrap (cursor == ""): sort_ascending=false + limit=1
//     fetches the single most-recently-modified task across the
//     project; newCursor equals that timestamp ("now"). Returns
//     zero changes.
//   - Steady-state (cursor != ""): sort_ascending=true +
//     modified_at.after=<cursor> + limit=100 returns the next
//     batch of changes oldest-first. max(modified_at) of this
//     page is always less than min(modified_at) of the next
//     call's response, so the next call's modified_at.after
//     filter picks up exactly where this one left off — no
//     skipped tasks regardless of changeset size. Mirrors the
//     monotonic-cursor invariant the HubSpot / Linear /
//     Salesforce DeltaSync paths already rely on, just via
//     a different endpoint because Asana's project list does
//     not sort by modification time.
//
// The workspace GID is stashed in Namespace.Metadata during
// ListNamespaces.
func (a *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("asana: bad connection type")
	}
	if cursor == "" {
		return a.deltaSyncBootstrap(ctx, conn, ns)
	}

	return a.deltaSyncSince(ctx, conn, ns, cursor)
}

func (a *Connector) deltaSyncBootstrap(ctx context.Context, conn *connection, ns connector.Namespace) ([]connector.DocumentChange, string, error) {
	wgid := ""
	if ns.Metadata != nil {
		wgid = ns.Metadata["workspace_gid"]
	}
	if wgid == "" {
		return nil, "", fmt.Errorf("%w: asana: namespace metadata workspace_gid required for delta bootstrap", connector.ErrInvalidConfig)
	}
	q := url.Values{}
	q.Set("projects.any", ns.ID)
	q.Set("sort_by", "modified_at")
	q.Set("sort_ascending", "false")
	q.Set("limit", "1")
	q.Set("opt_fields", "gid,modified_at")

	resp, err := a.do(ctx, conn, http.MethodGet, "/workspaces/"+wgid+"/tasks/search?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: asana: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("asana: workspace search status=%d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			GID        string `json:"gid"`
			ModifiedAt string `json:"modified_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("asana: decode workspace search: %w", err)
	}
	newCursor := ""
	if len(body.Data) > 0 {
		newCursor = body.Data[0].ModifiedAt
	}

	return nil, newCursor, nil
}

func (a *Connector) deltaSyncSince(ctx context.Context, conn *connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	wgid := ""
	if ns.Metadata != nil {
		wgid = ns.Metadata["workspace_gid"]
	}
	if wgid == "" {
		return nil, "", fmt.Errorf("%w: asana: namespace metadata workspace_gid required for delta sync", connector.ErrInvalidConfig)
	}
	q := url.Values{}
	q.Set("projects.any", ns.ID)
	q.Set("sort_by", "modified_at")
	q.Set("sort_ascending", "true")
	q.Set("modified_at.after", cursor)
	q.Set("limit", "100")
	q.Set("opt_fields", "gid,name,modified_at")

	resp, err := a.do(ctx, conn, http.MethodGet, "/workspaces/"+wgid+"/tasks/search?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Surface 429 as connector.ErrRateLimited so the adaptive rate
	// limiter reacts to Asana's workspace-search throttling during
	// delta sync, mirroring the bootstrap path and the
	// ListDocuments iterator above.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: asana: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("asana: workspace search status=%d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			GID        string `json:"gid"`
			ModifiedAt string `json:"modified_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("asana: decode workspace search: %w", err)
	}
	newCursor := cursor
	changes := make([]connector.DocumentChange, 0, len(body.Data))
	for _, t := range body.Data {
		updated, _ := time.Parse(time.RFC3339, t.ModifiedAt)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          t.GID,
				ETag:        t.ModifiedAt,
				UpdatedAt:   updated,
			},
		})
		if t.ModifiedAt > newCursor {
			newCursor = t.ModifiedAt
		}
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper.
func (a *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(a.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("asana: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asana: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the Asana connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
