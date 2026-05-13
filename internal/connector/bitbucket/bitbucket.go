// Package bitbucket implements the Bitbucket Cloud SourceConnector
// + DeltaSyncer against the Bitbucket Cloud REST API v2 at
// https://api.bitbucket.org. Authentication uses HTTP basic auth
// with a username + app password.
//
// Credentials must be a JSON blob:
//
//	{
//	 "username":     "...",   // required (Bitbucket username)
//	 "app_password": "...",   // required (Bitbucket app password)
//	 "workspace":    "...",   // required
//	 "repo":         "..."    // required
//	}
//
// The connector exposes the configured repo as the single
// namespace. Delta uses `/2.0/repositories/{ws}/{repo}/pullrequests`
// with `q=updated_on>"<ISO8601>"` and Bitbucket's standard
// pagination via the `next` link.
package bitbucket

import (
	"context"
	"encoding/base64"
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

// Name is the registry-visible connector name.
const Name = "bitbucket"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Username    string `json:"username"`
	AppPassword string `json:"app_password"`
	Workspace   string `json:"workspace"`
	Repo        string `json:"repo"`
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

// WithBaseURL pins the Bitbucket base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{httpClient: &http.Client{Timeout: 30 * time.Second}, baseURL: "https://api.bitbucket.org"}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID  string
	sourceID  string
	baseURL   string
	authHdr   string
	workspace string
	repo      string
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
	if creds.Username == "" || creds.AppPassword == "" {
		return fmt.Errorf("%w: username + app_password required", connector.ErrInvalidConfig)
	}
	if creds.Workspace == "" {
		return fmt.Errorf("%w: workspace required", connector.ErrInvalidConfig)
	}
	if creds.Repo == "" {
		return fmt.Errorf("%w: repo required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect verifies the repo is reachable.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.AppPassword))
	conn := &connection{
		tenantID:  cfg.TenantID,
		sourceID:  cfg.SourceID,
		baseURL:   o.baseURL,
		authHdr:   auth,
		workspace: creds.Workspace,
		repo:      creds.Repo,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/2.0/repositories/"+url.PathEscape(conn.workspace)+"/"+url.PathEscape(conn.repo), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: bitbucket: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bitbucket: repo status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the workspace/repo as a single namespace.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bitbucket: bad connection type")
	}

	return []connector.Namespace{{ID: conn.workspace + "/" + conn.repo, Name: conn.repo, Kind: "pullrequests"}}, nil
}

type prEntry struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	UpdatedOn string `json:"updated_on"`
	CreatedOn string `json:"created_on"`
	State     string `json:"state"`
	Summary   struct {
		Raw string `json:"raw"`
	} `json:"summary"`
}

type listResponse struct {
	Values []prEntry `json:"values"`
	Next   string    `json:"next"`
}

type docIterator struct {
	o    *Connector
	conn *connection
	ns   connector.Namespace
	page []connector.DocumentRef
	idx  int
	done bool
	err  error
	// nextPath holds the next page's path+query, parsed from
	// Bitbucket's `next` response field (a full URL). Empty
	// before the first fetch and once the API stops returning a
	// next URL. Round-22 pagination fix.
	nextPath string
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
	// Round-22 pagination fix: follow Bitbucket's `next` URL.
	path := it.nextPath
	if path == "" {
		q := url.Values{}
		q.Set("pagelen", "50")
		path = "/2.0/repositories/" + url.PathEscape(it.conn.workspace) + "/" + url.PathEscape(it.conn.repo) + "/pullrequests?" + q.Encode()
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: bitbucket: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("bitbucket: list status=%d", resp.StatusCode)

		return false
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("bitbucket: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, pr := range body.Values {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: strconv.FormatInt(pr.ID, 10)}
		if ts, perr := time.Parse(time.RFC3339, pr.UpdatedOn); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.nextPath = ""
	if body.Next != "" {
		if u, perr := url.Parse(body.Next); perr == nil && u.Path != "" {
			it.nextPath = u.RequestURI()
		}
	}
	it.done = it.nextPath == ""

	return true
}

// ListDocuments enumerates pull requests in the configured repo.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bitbucket: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a single pull request.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("bitbucket: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/2.0/repositories/"+url.PathEscape(conn.workspace)+"/"+url.PathEscape(conn.repo)+"/pullrequests/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: bitbucket: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bitbucket: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var pr prEntry
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("bitbucket: decode PR: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, pr.CreatedOn)
	updated, _ := time.Parse(time.RFC3339, pr.UpdatedOn)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: conn.workspace + "/" + conn.repo, ID: strconv.FormatInt(pr.ID, 10), UpdatedAt: updated},
		MIMEType:  "text/markdown",
		Title:     pr.Title,
		Size:      int64(len(pr.Summary.Raw)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(pr.Summary.Raw)),
		Metadata:  map[string]string{"state": pr.State},
	}, nil
}

// Subscribe is unsupported.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls pullrequests with q=updated_on>"<ISO8601>".
// Empty cursor returns "now" (bootstrap contract).
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("bitbucket: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("bitbucket: bad cursor %q: %w", cursor, err)
	}
	q := url.Values{}
	q.Set("q", `updated_on>"`+cursor+`"`)
	q.Set("sort", "updated_on")
	q.Set("pagelen", "50")
	q.Set("state", "OPEN")
	resp, err := o.do(ctx, conn, http.MethodGet, "/2.0/repositories/"+url.PathEscape(conn.workspace)+"/"+url.PathEscape(conn.repo)+"/pullrequests?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: bitbucket: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("bitbucket: delta status=%d", resp.StatusCode)
	}
	var body listResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("bitbucket: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, pr := range body.Values {
		ts, _ := time.Parse(time.RFC3339, pr.UpdatedOn)
		ts = ts.UTC()
		ref := connector.DocumentRef{NamespaceID: ns.ID, ID: strconv.FormatInt(pr.ID, 10), UpdatedAt: ts}
		changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		if ts.After(newest) {
			newest = ts
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHdr)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitbucket: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
