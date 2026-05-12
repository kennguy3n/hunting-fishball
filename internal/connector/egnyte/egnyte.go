// Package egnyte implements the Egnyte SourceConnector +
// DeltaSyncer against the Egnyte Public API at
// https://<domain>.egnyte.com/pubapi. Authentication is an OAuth
// 2.0 Bearer token; the platform's token-refresh worker handles
// renewal.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "...",  // required
//	  "domain":       "acme", // required (acme → https://acme.egnyte.com)
//	  "root_path":    "/Shared" // optional (default "/Shared")
//	}
//
// Delta is implemented via the Egnyte Events API at
// /pubapi/v2/events/cursor + /pubapi/v2/events; an empty cursor
// fetches the current cursor without backfilling.
package egnyte

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
	Name = "egnyte"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	Domain      string `json:"domain"`
	RootPath    string `json:"root_path,omitempty"`
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

// WithBaseURL pins the API base URL — used by tests.
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
	accessToken string
	rootPath    string
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
	if creds.AccessToken == "" {
		return fmt.Errorf("%w: access_token required", connector.ErrInvalidConfig)
	}
	if o.baseURL == "" && creds.Domain == "" {
		return fmt.Errorf("%w: domain required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /pubapi/v1/userinfo as the auth check.
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
		base = "https://" + creds.Domain + ".egnyte.com"
	}
	root := creds.RootPath
	if root == "" {
		root = "/Shared"
	}
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		baseURL:     base,
		accessToken: creds.AccessToken,
		rootPath:    root,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/pubapi/v1/userinfo", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: egnyte: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egnyte: userinfo status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the configured root folder as the only
// namespace. The Egnyte folder tree is recursive — children are
// surfaced via ListDocuments.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("egnyte: bad connection type")
	}

	return []connector.Namespace{{ID: conn.rootPath, Name: conn.rootPath, Kind: "folder"}}, nil
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

type fsListing struct {
	Files []struct {
		EntryID      string `json:"entry_id"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		Size         int64  `json:"size"`
		LastModified string `json:"last_modified"`
		Checksum     string `json:"checksum"`
	} `json:"files"`
}

func (it *docIterator) fetch(ctx context.Context) bool {
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/pubapi/v1/fs"+escapePath(it.ns.ID)+"?list_content=true", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: egnyte: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("egnyte: list status=%d", resp.StatusCode)

		return false
	}
	var body fsListing
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("egnyte: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, f := range body.Files {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: f.Path, ETag: f.Checksum}
		if f.LastModified != "" {
			if t, perr := http.ParseTime(f.LastModified); perr == nil {
				ref.UpdatedAt = t.UTC()
			}
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments walks the folder listing.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("egnyte: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument GETs file metadata + body. Egnyte exposes the
// download endpoint at /pubapi/v1/fs-content/<path>.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("egnyte: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/pubapi/v1/fs-content"+escapePath(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("%w: egnyte: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("egnyte: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("egnyte: read body: %w", err)
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  resp.Header.Get("Content-Type"),
		Title:     pathBase(ref.ID),
		Size:      int64(len(raw)),
		UpdatedAt: ref.UpdatedAt,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"checksum": resp.Header.Get("X-Sha512-Checksum")},
	}, nil
}

// Subscribe is unsupported — Egnyte change notifications are
// surfaced via the events polling API, which DeltaSync handles.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses the Egnyte Events API. The empty-cursor
// bootstrap fetches the current cursor via /pubapi/v2/events/cursor
// without returning any historical events. Subsequent calls fetch
// /pubapi/v2/events?id=<cursor> and walk the returned events.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("egnyte: bad connection type")
	}
	if cursor == "" {
		resp, err := o.do(ctx, conn, http.MethodGet, "/pubapi/v2/events/cursor", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: egnyte: cursor status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("egnyte: cursor status=%d", resp.StatusCode)
		}
		var body struct {
			LatestEventID json.Number `json:"latest_event_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("egnyte: decode cursor: %w", err)
		}
		return nil, body.LatestEventID.String(), nil
	}
	q := url.Values{}
	q.Set("id", cursor)
	resp, err := o.do(ctx, conn, http.MethodGet, "/pubapi/v2/events?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: egnyte: events status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("egnyte: events status=%d", resp.StatusCode)
	}
	var body struct {
		LatestID json.Number `json:"latest_id"`
		Events   []struct {
			ID     json.Number `json:"id"`
			Type   string      `json:"type"`
			Action string      `json:"action"`
			Data   struct {
				TargetPath string `json:"target_path"`
				ChangedAt  string `json:"timestamp"`
			} `json:"data"`
		} `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("egnyte: decode events: %w", err)
	}
	var changes []connector.DocumentChange
	for _, ev := range body.Events {
		if ev.Data.TargetPath == "" {
			continue
		}
		kind := connector.ChangeUpserted
		if strings.EqualFold(ev.Action, "delete") || strings.EqualFold(ev.Action, "deleted") {
			kind = connector.ChangeDeleted
		}
		ref := connector.DocumentRef{NamespaceID: ns.ID, ID: ev.Data.TargetPath}
		if ev.Data.ChangedAt != "" {
			if t, perr := time.Parse(time.RFC3339, ev.Data.ChangedAt); perr == nil {
				ref.UpdatedAt = t.UTC()
			}
		}
		changes = append(changes, connector.DocumentChange{Kind: kind, Ref: ref})
	}
	newCur := body.LatestID.String()
	if newCur == "" {
		newCur = cursor
	}

	return changes, newCur, nil
}

// do builds and sends an authenticated HTTP request.
func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("egnyte: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("egnyte: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// escapePath escapes each path segment with url.PathEscape while
// preserving the leading slash. Egnyte paths are reported in the
// /Shared/foo/bar form.
func escapePath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}

	return strings.Join(parts, "/")
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}

	return p
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
