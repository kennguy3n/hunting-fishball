// Package googledrive implements the Google Drive SourceConnector.
//
// The connector talks to Google Drive REST API v3. We use a thin
// stdlib HTTP wrapper rather than google.golang.org/api/drive/v3 to
// keep the dependency surface flat and the unit tests purely
// httptest-driven. Future phases can swap to the official SDK without
// touching SourceConnector callers.
//
// Credentials must be a JSON blob with at least:
//
//	{
//	  "access_token": "...",
//	  "refresh_token": "...",   // optional
//	  "token_uri":   "..."       // optional, defaults to Google's
//	}
//
// The control-plane decrypts internal/credential storage before
// supplying the bytes to Connect.
package googledrive

import (
	"bytes"
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
	Name = "google_drive"

	defaultBaseURL = "https://www.googleapis.com/drive/v3"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenURI     string `json:"token_uri,omitempty"`
	Expiry       string `json:"expiry,omitempty"`
}

// Connector is the SourceConnector implementation.
type Connector struct {
	httpClient *http.Client
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient sets the underlying HTTP client. Tests inject the
// httptest.Server's client.
func WithHTTPClient(c *http.Client) Option {
	return func(g *Connector) { g.httpClient = c }
}

// WithBaseURL overrides the Drive API base URL. Used by tests.
func WithBaseURL(u string) Option {
	return func(g *Connector) { g.baseURL = u }
}

// New constructs a Connector.
func New(opts ...Option) *Connector {
	g := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: defaultBaseURL}
	for _, opt := range opts {
		opt(g)
	}

	return g
}

// connection holds the per-call state derived from ConnectorConfig.
type connection struct {
	tenantID    string
	sourceID    string
	accessToken string
	credentials Credentials
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses the credential blob and surfaces a clear error on
// malformed configs.
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

// Connect performs a cheap auth check by calling Drive's `about`
// endpoint, then returns a Connection bound to the tenant.
func (g *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := g.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}

	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		accessToken: creds.AccessToken,
		credentials: creds,
	}

	resp, err := g.do(ctx, conn, http.MethodGet, "/about?fields=user", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("googledrive: auth check failed: status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the visible drives plus a synthetic "My
// Drive" namespace. Shared drives are paginated via
// `nextPageToken`; large shared-drive sets (>100 drives) are
// followed to completion. Each drive page is requested with
// `useDomainAdminAccess=false` by default — Round-15 hardening
// guarantees we surface every drive the calling user can see,
// not just the first page.
func (g *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("googledrive: bad connection type")
	}

	out := []connector.Namespace{
		{ID: "my-drive", Name: "My Drive", Kind: "my_drive"},
	}

	pageToken := ""
	for {
		// Use url.Values so pageToken is properly encoded —
		// Google pageToken values are opaque and may contain
		// characters (=, +, /) that break raw concatenation,
		// silently truncating the shared-drive list. Matches
		// the document iterator below.
		q := url.Values{}
		q.Set("pageSize", "100")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		path := "/drives?" + q.Encode()
		resp, err := g.do(ctx, conn, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("%w: googledrive: listing drives: status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("googledrive: list drives status=%d", resp.StatusCode)
		}
		var body struct {
			NextPageToken string `json:"nextPageToken"`
			Drives        []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"drives"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, fmt.Errorf("googledrive: decode drives: %w", err)
		}
		_ = resp.Body.Close()
		for _, d := range body.Drives {
			out = append(out, connector.Namespace{
				ID: d.ID, Name: d.Name, Kind: "shared_drive",
				Metadata: map[string]string{"drive_id": d.ID},
			})
		}
		if body.NextPageToken == "" {
			break
		}
		pageToken = body.NextPageToken
	}

	return out, nil
}

// docIterator implements connector.DocumentIterator backed by paginated
// Drive `files.list` calls.
type docIterator struct {
	g       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
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

func (it *docIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("fields", "nextPageToken,files(id,name,mimeType,modifiedTime,size,version)")
	q.Set("pageSize", strconv.Itoa(pageOr(it.opts.PageSize, 100)))
	q.Set("orderBy", "modifiedTime desc")
	if it.cursor != "" {
		q.Set("pageToken", it.cursor)
	} else if it.opts.PageToken != "" {
		q.Set("pageToken", it.opts.PageToken)
	}
	if it.ns.ID != "my-drive" {
		q.Set("driveId", it.ns.ID)
		q.Set("includeItemsFromAllDrives", "true")
		q.Set("supportsAllDrives", "true")
		q.Set("corpora", "drive")
	} else {
		q.Set("corpora", "user")
	}
	if !it.opts.Since.IsZero() {
		q.Set("q", fmt.Sprintf("modifiedTime > '%s'", it.opts.Since.UTC().Format(time.RFC3339)))
	}

	resp, err := it.g.do(ctx, it.conn, http.MethodGet, "/files?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("googledrive: list files status=%d", resp.StatusCode)

		return false
	}

	var body struct {
		NextPageToken string `json:"nextPageToken"`
		Files         []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			ModifiedTime string `json:"modifiedTime"`
			Size         string `json:"size"`
			Version      string `json:"version"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("googledrive: decode files: %w", err)

		return false
	}

	it.page = it.page[:0]
	it.pageIdx = 0
	for _, f := range body.Files {
		modified, _ := time.Parse(time.RFC3339, f.ModifiedTime)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          f.ID,
			ETag:        f.Version,
			UpdatedAt:   modified,
		})
	}
	if body.NextPageToken == "" {
		it.done = true

		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.NextPageToken
	}

	return true
}

// ListDocuments returns a paginated iterator.
func (g *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("googledrive: bad connection type")
	}

	return &docIterator{g: g, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument calls files.get with alt=media to download the body.
func (g *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("googledrive: bad connection type")
	}

	// Fetch metadata first.
	metaResp, err := g.do(ctx, conn, http.MethodGet, "/files/"+url.PathEscape(ref.ID)+"?fields=id,name,mimeType,size,modifiedTime,createdTime,owners(emailAddress,displayName)", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = metaResp.Body.Close() }()
	if metaResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("googledrive: get file status=%d", metaResp.StatusCode)
	}

	var meta struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		MimeType     string `json:"mimeType"`
		Size         string `json:"size"`
		ModifiedTime string `json:"modifiedTime"`
		CreatedTime  string `json:"createdTime"`
		Owners       []struct {
			Email string `json:"emailAddress"`
			Name  string `json:"displayName"`
		} `json:"owners"`
	}
	if err := json.NewDecoder(metaResp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("googledrive: decode meta: %w", err)
	}

	bodyResp, err := g.do(ctx, conn, http.MethodGet, "/files/"+url.PathEscape(ref.ID)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	if bodyResp.StatusCode != http.StatusOK {
		_ = bodyResp.Body.Close()

		return nil, fmt.Errorf("googledrive: download status=%d", bodyResp.StatusCode)
	}

	created, _ := time.Parse(time.RFC3339, meta.CreatedTime)
	modified, _ := time.Parse(time.RFC3339, meta.ModifiedTime)
	size, _ := strconv.ParseInt(meta.Size, 10, 64)

	owner := ""
	if len(meta.Owners) > 0 {
		owner = strings.TrimSpace(meta.Owners[0].Email)
		if owner == "" {
			owner = meta.Owners[0].Name
		}
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  meta.MimeType,
		Title:     meta.Name,
		Author:    owner,
		Size:      size,
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   bodyResp.Body,
		Metadata:  map[string]string{"file_id": ref.ID, "drive_namespace": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported for Drive — callers fall back to DeltaSync.
func (g *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op (HTTP is stateless).
func (g *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync implements connector.DeltaSyncer using Drive's changes.list
// endpoint. cursor is the Drive `startPageToken`. An empty cursor
// returns the current token without backfilling, matching the
// connector contract.
func (g *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("googledrive: bad connection type")
	}
	if cursor == "" {
		// First call: fetch the current start token.
		resp, err := g.do(ctx, conn, http.MethodGet, "/changes/startPageToken", nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("googledrive: startPageToken status=%d", resp.StatusCode)
		}
		var body struct {
			StartPageToken string `json:"startPageToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return nil, "", fmt.Errorf("googledrive: decode startPageToken: %w", err)
		}

		return nil, body.StartPageToken, nil
	}

	q := url.Values{}
	q.Set("pageToken", cursor)
	q.Set("fields", "nextPageToken,newStartPageToken,changes(fileId,removed,file(id,modifiedTime))")
	if ns.ID != "my-drive" {
		q.Set("driveId", ns.ID)
		q.Set("includeItemsFromAllDrives", "true")
		q.Set("supportsAllDrives", "true")
	}

	resp, err := g.do(ctx, conn, http.MethodGet, "/changes?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("googledrive: changes status=%d", resp.StatusCode)
	}

	var body struct {
		NextPageToken     string `json:"nextPageToken"`
		NewStartPageToken string `json:"newStartPageToken"`
		Changes           []struct {
			FileID  string `json:"fileId"`
			Removed bool   `json:"removed"`
			File    struct {
				ID           string `json:"id"`
				ModifiedTime string `json:"modifiedTime"`
			} `json:"file"`
		} `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("googledrive: decode changes: %w", err)
	}

	changes := make([]connector.DocumentChange, 0, len(body.Changes))
	for _, ch := range body.Changes {
		kind := connector.ChangeUpserted
		if ch.Removed {
			kind = connector.ChangeDeleted
		}
		modified, _ := time.Parse(time.RFC3339, ch.File.ModifiedTime)
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: ch.FileID, UpdatedAt: modified},
		})
	}

	next := body.NextPageToken
	if next == "" {
		next = body.NewStartPageToken
	}
	if next == "" {
		next = cursor
	}

	return changes, next, nil
}

// do is the shared HTTP path. Adds auth, parses JSON errors.
func (g *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(g.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("googledrive: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("googledrive: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// pageOr returns v when the caller supplied a positive page size and
// otherwise falls back to the connector default. ListOpts.PageSize
// documents "zero means connector default", so this must NOT clamp
// caller-supplied values upward — that would silently override the
// caller's preference.
func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// jsonReader is a small helper for tests that need to construct a body
// without pulling in extra deps.
func jsonReader(v any) io.Reader { //nolint:unused
	b, _ := json.Marshal(v)

	return bytes.NewReader(b)
}

// Register registers the Google Drive connector with the global
// registry. The init function calls this at import time.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
