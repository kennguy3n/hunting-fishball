// Package sharepointonprem implements the SharePoint Server /
// Data Center SourceConnector + DeltaSyncer against the on-prem
// REST API at https://<host>/_api/web. Authentication uses
// either NTLM (username + password against the SharePoint Web
// Application's IIS) or a SharePoint app-password (Bearer token
// against /_api when SharePoint is configured to accept OAuth).
//
// Credentials must be a JSON blob:
//
//	{
//	  "base_url":    "https://sp.acme.com",  // required
//	  "site_path":   "/sites/eng",           // optional, default ""
//	  "app_password":"...",                  // optional (Bearer)
//	  "username":    "ALICE",                // optional (NTLM)
//	  "password":    "..."                   // optional (NTLM)
//	}
//
// Exactly one of app_password OR (username+password) must be
// set. Delta is implemented via the SharePoint REST OData filter
// `Modified gt datetime'<ISO8601>'` against `/_api/web/lists`.
//
// We stay on stdlib net/http only — NTLM here is implemented as
// `WWW-Authenticate` basic-auth challenge fallback (the SharePoint
// Server farms we target accept the Basic / Negotiate header).
// For test purposes httptest serves the canned responses; the
// connector treats Basic auth as the on-the-wire credential.
package sharepointonprem

import (
	"context"
	"encoding/base64"
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
	Name = "sharepoint_onprem"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	BaseURL     string `json:"base_url"`
	SitePath    string `json:"site_path,omitempty"`
	AppPassword string `json:"app_password,omitempty"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
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

// WithBaseURL pins the SharePoint base URL — used by tests.
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
	sitePath    string
	appPassword string
	username    string
	password    string
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
	if creds.AppPassword == "" && (creds.Username == "" || creds.Password == "") {
		return fmt.Errorf("%w: app_password or username+password required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect calls /_api/web as the auth check.
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
		sitePath:    creds.SitePath,
		appPassword: creds.AppPassword,
		username:    creds.Username,
		password:    creds.Password,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/_api/web?$select=Title", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: sharepoint_onprem: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sharepoint_onprem: /_api/web status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the SharePoint Lists as namespaces.
func (o *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sharepoint_onprem: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/_api/web/lists?$select=Id,Title", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: sharepoint_onprem: lists status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sharepoint_onprem: lists status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID    string `json:"Id"`
			Title string `json:"Title"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("sharepoint_onprem: decode lists: %w", err)
	}
	out := make([]connector.Namespace, 0, len(body.Value))
	for _, l := range body.Value {
		out = append(out, connector.Namespace{ID: l.ID, Name: l.Title, Kind: "list"})
	}

	return out, nil
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	opts connector.ListOpts
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
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/_api/web/lists(guid'"+url.PathEscape(it.ns.ID)+"')/items?$select=Id,Modified", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: sharepoint_onprem: items status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("sharepoint_onprem: items status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Value []struct {
			ID       int    `json:"Id"`
			Modified string `json:"Modified"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("sharepoint_onprem: decode items: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, v := range body.Value {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: fmt.Sprintf("%d", v.ID)}
		if t, err := time.Parse(time.RFC3339, v.Modified); err == nil {
			ref.UpdatedAt = t
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments enumerates list items.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sharepoint_onprem: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument loads a single list item.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("sharepoint_onprem: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/_api/web/lists(guid'"+url.PathEscape(ref.NamespaceID)+"')/items("+url.PathEscape(ref.ID)+")", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: sharepoint_onprem: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sharepoint_onprem: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("sharepoint_onprem: read body: %w", err)
	}
	var body struct {
		Title    string `json:"Title"`
		Author   string `json:"AuthorId"`
		Created  string `json:"Created"`
		Modified string `json:"Modified"`
	}
	_ = json.Unmarshal(raw, &body)
	created, _ := time.Parse(time.RFC3339, body.Created)
	modified, _ := time.Parse(time.RFC3339, body.Modified)
	if modified.IsZero() {
		modified = ref.UpdatedAt
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     body.Title,
		Author:    body.Author,
		Size:      int64(len(raw)),
		CreatedAt: created,
		UpdatedAt: modified,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"kind": "list_item"},
	}, nil
}

// Subscribe is unsupported — SharePoint Server change notifications
// require webhook plumbing the platform doesn't yet mount.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /_api/web/lists(guid'<id>')/items with the OData
// filter `Modified gt datetime'<ISO8601>'`. The empty-cursor
// contract returns the upstream's current high-water mark
// without backfilling.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("sharepoint_onprem: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("sharepoint_onprem: bad cursor %q: %w", cursor, err)
	}
	q := url.Values{}
	q.Set("$select", "Id,Modified")
	q.Set("$filter", "Modified gt datetime'"+cursorTime.UTC().Format(time.RFC3339)+"'")
	resp, err := o.do(ctx, conn, http.MethodGet, "/_api/web/lists(guid'"+url.PathEscape(ns.ID)+"')/items?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: sharepoint_onprem: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("sharepoint_onprem: delta status=%d", resp.StatusCode)
	}
	var body struct {
		Value []struct {
			ID       int    `json:"Id"`
			Modified string `json:"Modified"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("sharepoint_onprem: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, v := range body.Value {
		t, perr := time.Parse(time.RFC3339, v.Modified)
		if perr != nil {
			continue
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: fmt.Sprintf("%d", v.ID), UpdatedAt: t},
		})
		if t.After(newest) {
			newest = t
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// do is the shared HTTP helper. Wraps the base URL + site path
// and applies either Basic (username:password) or Bearer
// (app_password) auth.
func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + strings.TrimRight(conn.sitePath, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("sharepoint_onprem: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json;odata=verbose")
	if conn.appPassword != "" {
		req.Header.Set("Authorization", "Bearer "+conn.appPassword)
	} else {
		auth := conn.username + ":" + conn.password
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json;odata=verbose")
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sharepoint_onprem: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
