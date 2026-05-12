// Package hubspot implements the HubSpot SourceConnector +
// DeltaSyncer against the HubSpot CRM REST API v3 at
// https://api.hubapi.com. Authentication uses a private-app
// bearer token. We use stdlib net/http only.
//
// Credentials must be a JSON blob:
//
//	{ "access_token": "pat-..." }   // required
package hubspot

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
	Name = "hubspot"

	defaultBaseURL = "https://api.hubapi.com"
)

// defaultObjects is the curated set of CRM object types HubSpot
// exposes by default. Tenants can extend via WithObjects.
var defaultObjects = []string{"contacts", "companies", "deals", "tickets"}

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessToken string `json:"access_token"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	baseURL    string
	objects    []string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(h *Connector) { h.httpClient = c } }

// WithBaseURL overrides the HubSpot API base URL — used by tests.
func WithBaseURL(u string) Option { return func(h *Connector) { h.baseURL = u } }

// WithObjects overrides the curated list of object types exposed
// as namespaces.
func WithObjects(objs []string) Option { return func(h *Connector) { h.objects = objs } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	h := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
		objects:    append([]string{}, defaultObjects...),
	}
	for _, opt := range opts {
		opt(h)
	}

	return h
}

type connection struct {
	tenantID string
	sourceID string
	token    string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (h *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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

// Connect calls /integrations/v1/me as the auth check (returns
// the connected portal ID with a minimal access scope).
func (h *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := h.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, token: creds.AccessToken}

	resp, err := h.do(ctx, conn, http.MethodGet, "/integrations/v1/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hubspot: /integrations/v1/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the curated object types as namespaces.
func (h *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	if _, ok := c.(*connection); !ok {
		return nil, errors.New("hubspot: bad connection type")
	}
	out := make([]connector.Namespace, 0, len(h.objects))
	for _, o := range h.objects {
		out = append(out, connector.Namespace{
			ID: o, Name: o, Kind: "crm_object",
			Metadata: map[string]string{"object_type": o},
		})
	}

	return out, nil
}

// crmIterator paginates over /crm/v3/objects/{type}.
type crmIterator struct {
	h       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
	err     error
	done    bool
}

func (it *crmIterator) Next(ctx context.Context) bool {
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

func (it *crmIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *crmIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *crmIterator) Close() error { return nil }

func (it *crmIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(pageOr(it.opts.PageSize, 100)))
	if it.cursor != "" {
		q.Set("after", it.cursor)
	} else if it.opts.PageToken != "" {
		q.Set("after", it.opts.PageToken)
	}

	resp, err := it.h.do(ctx, it.conn, http.MethodGet, "/crm/v3/objects/"+it.ns.ID+"?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: hubspot: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("hubspot: crm objects status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Results []struct {
			ID         string `json:"id"`
			UpdatedAt  string `json:"updatedAt"`
			Properties map[string]string
		} `json:"results"`
		Paging struct {
			Next struct {
				After string `json:"after"`
			} `json:"next"`
		} `json:"paging"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("hubspot: decode crm: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, r := range body.Results {
		updated, _ := time.Parse(time.RFC3339, r.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          r.ID,
			ETag:        r.UpdatedAt,
			UpdatedAt:   updated,
		})
	}
	if body.Paging.Next.After == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.Paging.Next.After
	}

	return true
}

// ListDocuments enumerates CRM objects of the namespace's type.
func (h *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("hubspot: bad connection type")
	}

	return &crmIterator{h: h, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single CRM object by ID.
func (h *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("hubspot: bad connection type")
	}
	resp, err := h.do(ctx, conn, http.MethodGet, "/crm/v3/objects/"+ref.NamespaceID+"/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("hubspot: get object status=%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("hubspot: read body: %w", err)
	}
	var body struct {
		ID         string            `json:"id"`
		UpdatedAt  string            `json:"updatedAt"`
		CreatedAt  string            `json:"createdAt"`
		Properties map[string]string `json:"properties"`
	}
	_ = json.Unmarshal(raw, &body)
	updated, _ := time.Parse(time.RFC3339, body.UpdatedAt)
	created, _ := time.Parse(time.RFC3339, body.CreatedAt)
	title := body.Properties["name"]
	if title == "" {
		title = body.Properties["email"]
	}
	if title == "" {
		title = ref.NamespaceID + "/" + ref.ID
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"object_type": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — HubSpot pushes change events through
// its webhook subscription system.
func (h *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (h *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses the search API with a `hs_lastmodifieddate >
// cursor` filter. The cursor is an ISO-8601 timestamp; the new
// cursor returned is the latest hs_lastmodifieddate seen. Empty
// cursor on the first call returns the current cursor without
// backfilling — to honor that contract we must bootstrap with the
// single most-recently-modified record (DESCENDING + limit=1) so
// newCursor represents "now". With ASCENDING + limit=100, the
// first page is the 100 oldest records and newCursor would be
// years stale, causing the next call to backfill nearly the entire
// dataset as "changes".
//
// Subsequent calls (cursor != "") use ASCENDING so the page starts
// with the oldest unseen change, giving stable forward-only cursor
// advancement under the GT filter.
func (h *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("hubspot: bad connection type")
	}
	var req struct {
		FilterGroups []struct {
			Filters []struct {
				PropertyName string `json:"propertyName"`
				Operator     string `json:"operator"`
				Value        string `json:"value"`
			} `json:"filters"`
		} `json:"filterGroups,omitempty"`
		Sorts      []map[string]string `json:"sorts"`
		Properties []string            `json:"properties"`
		Limit      int                 `json:"limit"`
	}
	req.Properties = []string{"hs_lastmodifieddate"}
	if cursor == "" {
		req.Sorts = []map[string]string{{"propertyName": "hs_lastmodifieddate", "direction": "DESCENDING"}}
		req.Limit = 1
	} else {
		req.Sorts = []map[string]string{{"propertyName": "hs_lastmodifieddate", "direction": "ASCENDING"}}
		req.Limit = 100
		req.FilterGroups = append(req.FilterGroups, struct {
			Filters []struct {
				PropertyName string `json:"propertyName"`
				Operator     string `json:"operator"`
				Value        string `json:"value"`
			} `json:"filters"`
		}{Filters: []struct {
			PropertyName string `json:"propertyName"`
			Operator     string `json:"operator"`
			Value        string `json:"value"`
		}{{PropertyName: "hs_lastmodifieddate", Operator: "GT", Value: cursor}}})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", fmt.Errorf("hubspot: encode search: %w", err)
	}
	resp, err := h.do(ctx, conn, http.MethodPost, "/crm/v3/objects/"+ns.ID+"/search", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Surface 429 as connector.ErrRateLimited so the adaptive rate
	// limiter reacts to HubSpot's burst-and-daily quota during delta
	// sync, mirroring the ListDocuments iterator above.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: hubspot: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("hubspot: search status=%d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			ID         string `json:"id"`
			UpdatedAt  string `json:"updatedAt"`
			Properties struct {
				HSLastmodified string `json:"hs_lastmodifieddate"`
			} `json:"properties"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("hubspot: decode search: %w", err)
	}
	changes := make([]connector.DocumentChange, 0, len(out.Results))
	newCursor := cursor
	for _, r := range out.Results {
		updated, _ := time.Parse(time.RFC3339, r.UpdatedAt)
		ts := r.UpdatedAt
		if r.Properties.HSLastmodified != "" {
			ts = r.Properties.HSLastmodified
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          r.ID,
				ETag:        ts,
				UpdatedAt:   updated,
			},
		})
		if ts > newCursor {
			newCursor = ts
		}
	}
	if cursor == "" {
		changes = nil
	}

	return changes, newCursor, nil
}

// do is the shared HTTP helper.
func (h *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(h.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("hubspot: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hubspot: %s %s: %w", method, target, err)
	}

	return resp, nil
}

func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the HubSpot connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
