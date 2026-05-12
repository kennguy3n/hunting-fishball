// Package gcs implements the Google Cloud Storage SourceConnector +
// DeltaSyncer against the JSON API at
// https://storage.googleapis.com/storage/v1. Authentication is a
// pre-resolved OAuth 2.0 access token (Bearer); the platform's
// existing token-refresh worker handles renewal.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "...",  // required
//	  "bucket":       "..."   // required
//	}
//
// Delta is implemented via the JSON API `updated` field on each
// object — we surface objects whose `updated` is strictly greater
// than the cursor.
package gcs

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
	Name = "gcs"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
	Bucket      string `json:"bucket"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
	now        func() time.Time
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(o *Connector) { o.httpClient = c } }

// WithBaseURL pins the storage endpoint — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// WithNow injects a deterministic clock used by tests so the
// bootstrap cursor is reproducible.
func WithNow(now func() time.Time) Option { return func(o *Connector) { o.now = now } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://storage.googleapis.com",
		now:        func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID    string
	sourceID    string
	accessToken string
	bucket      string
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
	if creds.Bucket == "" {
		return fmt.Errorf("%w: bucket required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues a HEAD-equivalent (GET /b/<bucket>) to verify
// auth.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
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
		bucket:      creds.Bucket,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/storage/v1/b/"+url.PathEscape(conn.bucket), "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: gcs: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gcs: GET /b/%s status=%d", conn.bucket, resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces exposes the configured bucket as a single
// namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gcs: bad connection type")
	}

	return []connector.Namespace{{ID: conn.bucket, Name: conn.bucket, Kind: "bucket"}}, nil
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

type listObjectsResp struct {
	Items []struct {
		Name        string `json:"name"`
		ContentType string `json:"contentType"`
		Size        string `json:"size"`
		Updated     string `json:"updated"`
		ETag        string `json:"etag"`
		MD5Hash     string `json:"md5Hash"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

func (it *docIterator) fetch(ctx context.Context) bool {
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/storage/v1/b/"+url.PathEscape(it.conn.bucket)+"/o", "")
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: gcs: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("gcs: list status=%d", resp.StatusCode)

		return false
	}
	var body listObjectsResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("gcs: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, b := range body.Items {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: b.Name, ETag: b.ETag}
		if t, perr := time.Parse(time.RFC3339, b.Updated); perr == nil {
			ref.UpdatedAt = t.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments enumerates objects in the bucket.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gcs: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads object metadata and the media body via the
// JSON API's `alt=media` mode.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("gcs: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/storage/v1/b/"+url.PathEscape(conn.bucket)+"/o/"+url.PathEscape(ref.ID), "alt=media")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("%w: gcs: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("gcs: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("gcs: read body: %w", err)
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  resp.Header.Get("Content-Type"),
		Title:     ref.ID,
		Size:      int64(len(raw)),
		UpdatedAt: ref.UpdatedAt,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"etag": resp.Header.Get("ETag")},
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync surfaces objects whose `updated` field is strictly
// greater than the cursor.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("gcs: bad connection type")
	}
	if cursor == "" {
		return nil, o.now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("gcs: bad cursor %q: %w", cursor, err)
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/storage/v1/b/"+url.PathEscape(conn.bucket)+"/o", "")
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: gcs: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("gcs: delta status=%d", resp.StatusCode)
	}
	var body listObjectsResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("gcs: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, b := range body.Items {
		t, perr := time.Parse(time.RFC3339, b.Updated)
		if perr != nil {
			continue
		}
		t = t.UTC()
		if !t.After(cursorTime) {
			continue
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: b.Name, ETag: b.ETag, UpdatedAt: t},
		})
		if t.After(newest) {
			newest = t
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// do builds and sends an HTTP request against the GCS JSON API.
// rawQuery is the query string sans `?`.
func (o *Connector) do(ctx context.Context, conn *connection, method, path, rawQuery string) (*http.Response, error) {
	target := strings.TrimRight(o.baseURL, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("gcs: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gcs: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
