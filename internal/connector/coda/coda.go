// Package coda implements the Coda SourceConnector +
// DeltaSyncer against the Coda v1 REST API at
// https://coda.io/apis/v1. Authentication uses a bearer API
// token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_token": "..." // required
//	}
//
// The connector exposes a single synthetic namespace "docs". On
// each call we list /docs (paged via `pageToken`); FetchDocument
// returns each Coda document's metadata + a manifest of its
// pages from /docs/{id}/pages. DeltaSync uses the `updatedAt`
// field on /docs (sorted DESC) as a high-water cursor and
// surfaces documents missing from the listing as
// `connector.ChangeDeleted` only if a future round adds explicit
// tombstone support — for now, all observed entries are upserts.
package coda

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
	Name = "coda"

	defaultBaseURL = "https://coda.io/apis/v1"
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
func WithHTTPClient(c *http.Client) Option { return func(o *Connector) { o.httpClient = c } }

// WithBaseURL pins the Coda base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID string
	sourceID string
	token    string
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

	return nil
}

// Connect calls /whoami as an auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken}
	resp, err := o.do(ctx, conn, http.MethodGet, "/whoami", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: coda: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coda: /whoami status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns a single synthetic namespace "docs".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "docs", Name: "Docs", Kind: "docs"}}, nil
}

type docIterator struct {
	o         *Connector
	conn      *connection
	ns        connector.Namespace
	opts      connector.ListOpts
	page      []connector.DocumentRef
	pageIdx   int
	nextToken string
	first     bool
	err       error
}

func (it *docIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	if it.pageIdx >= len(it.page) {
		if it.first && it.nextToken == "" {
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
	if it.err == nil && it.first && it.nextToken == "" && it.pageIdx >= len(it.page) {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *docIterator) Close() error { return nil }

func (it *docIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("limit", "100")
	if it.nextToken != "" {
		q.Set("pageToken", it.nextToken)
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/docs?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: coda: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("coda: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("coda: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, d := range body.Items {
		it.page = append(it.page, connector.DocumentRef{NamespaceID: it.ns.ID, ID: d.ID})
	}
	it.nextToken = body.NextPageToken
	it.first = true

	return true
}

// ListDocuments enumerates docs from /docs.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("coda: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single doc's metadata and an inline
// listing of its first page of /docs/{id}/pages so downstream
// callers can locate page IDs without a second round-trip.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("coda: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/docs/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: coda: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coda: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("coda: read body: %w", err)
	}
	var body struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Owner     string `json:"owner"`
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}
	_ = json.Unmarshal(raw, &body)
	created, _ := time.Parse(time.RFC3339, body.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, body.UpdatedAt)
	if updated.IsZero() {
		updated = created
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     body.Name,
		Author:    body.Owner,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": "doc"},
	}, nil
}

// Subscribe is unsupported — Coda webhook configuration is
// handled per-doc via the Coda UI / Pack integrations.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses `/docs?sortBy=updatedAt&direction=DESC&limit=1`
// for the bootstrap-no-backfill cursor, then on subsequent calls
// pages /docs sorted DESC and stops once it sees a doc with
// `updatedAt <= cursor`.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("coda: bad connection type")
	}
	if cursor == "" {
		q := url.Values{}
		q.Set("limit", "1")
		q.Set("sortBy", "updatedAt")
		q.Set("direction", "DESC")
		resp, err := o.do(ctx, conn, http.MethodGet, "/docs?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: coda: bootstrap status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("coda: bootstrap status=%d", resp.StatusCode)
		}
		var body struct {
			Items []struct {
				UpdatedAt string `json:"updatedAt"`
			} `json:"items"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		ts := time.Now().UTC().Format(time.RFC3339)
		if len(body.Items) > 0 && body.Items[0].UpdatedAt != "" {
			if t, err := time.Parse(time.RFC3339, body.Items[0].UpdatedAt); err == nil {
				ts = t.UTC().Format(time.RFC3339)
			}
		}

		return nil, ts, nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("coda: bad cursor %q: %w", cursor, err)
	}
	var changes []connector.DocumentChange
	newest := cursorTime
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("sortBy", "updatedAt")
		q.Set("direction", "DESC")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		resp, err := o.do(ctx, conn, http.MethodGet, "/docs?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("%w: coda: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()

			return nil, cursor, fmt.Errorf("coda: delta status=%d", resp.StatusCode)
		}
		var body struct {
			Items []struct {
				ID        string `json:"id"`
				UpdatedAt string `json:"updatedAt"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			_ = resp.Body.Close()

			return nil, "", fmt.Errorf("coda: decode delta: %w", err)
		}
		_ = resp.Body.Close()
		stop := false
		for _, it := range body.Items {
			t, perr := time.Parse(time.RFC3339, it.UpdatedAt)
			if perr != nil {
				continue
			}
			if !t.After(cursorTime) {
				stop = true

				break
			}
			changes = append(changes, connector.DocumentChange{
				Kind: connector.ChangeUpserted,
				Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: it.ID, UpdatedAt: t},
			})
			if t.After(newest) {
				newest = t
			}
		}
		if stop || body.NextPageToken == "" {
			break
		}
		pageToken = body.NextPageToken
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// do is the shared HTTP helper.
func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(o.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("coda: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coda: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the Coda connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
