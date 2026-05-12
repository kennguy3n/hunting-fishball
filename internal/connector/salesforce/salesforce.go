// Package salesforce implements the Salesforce SourceConnector +
// DeltaSyncer. Salesforce exposes a REST API rooted at
// `<instance_url>/services/data/v59.0` with SOQL query support
// via /query and pagination via the `nextRecordsUrl` field. We
// use OAuth 2.0 Bearer tokens (the control plane handles token
// refresh — this connector treats the supplied access_token as
// opaque) and pure stdlib net/http.
//
// Credentials must be a JSON blob:
//
//	{
//	  "instance_url": "https://acme.my.salesforce.com",   // required
//	  "access_token": "00D..."                              // required
//	}
//
// Settings may include `api_version` to override "v59.0".
package salesforce

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
	Name = "salesforce"

	defaultAPIVersion = "v59.0"
)

// defaultObjects is the curated set of SObjects we surface as
// namespaces. The real Salesforce schema is open-ended; we list
// the ones the retrieval pipeline cares about by default.
var defaultObjects = []string{"Account", "Contact", "Opportunity", "Case", "Lead"}

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	InstanceURL string `json:"instance_url"`
	AccessToken string `json:"access_token"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	apiVersion string
	objects    []string
	baseURL    string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(s *Connector) { s.httpClient = c } }

// WithAPIVersion overrides the Salesforce REST API version.
func WithAPIVersion(v string) Option { return func(s *Connector) { s.apiVersion = v } }

// WithObjects overrides the curated list of SObjects exposed as
// namespaces. Useful for tests and for tenants that want extras.
func WithObjects(objs []string) Option { return func(s *Connector) { s.objects = objs } }

// WithBaseURL overrides the instance URL — used by tests so the
// real Salesforce SDK isn't needed.
func WithBaseURL(u string) Option { return func(s *Connector) { s.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	s := &Connector{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		apiVersion: defaultAPIVersion,
		objects:    append([]string{}, defaultObjects...),
	}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

type connection struct {
	tenantID    string
	sourceID    string
	token       string
	instanceURL string
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
	var c Credentials
	if err := json.Unmarshal(cfg.Credentials, &c); err != nil {
		return fmt.Errorf("%w: parse credentials: %v", connector.ErrInvalidConfig, err)
	}
	if c.InstanceURL == "" {
		return fmt.Errorf("%w: instance_url required", connector.ErrInvalidConfig)
	}
	if c.AccessToken == "" {
		return fmt.Errorf("%w: access_token required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect runs /services/data/<v>/limits as a cheap auth check.
func (s *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := s.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{
		tenantID:    cfg.TenantID,
		sourceID:    cfg.SourceID,
		token:       creds.AccessToken,
		instanceURL: s.resolveInstance(creds.InstanceURL),
	}

	resp, err := s.do(ctx, conn, http.MethodGet, "/limits", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("salesforce: limits status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the curated SObjects as namespaces.
func (s *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	if _, ok := c.(*connection); !ok {
		return nil, errors.New("salesforce: bad connection type")
	}
	out := make([]connector.Namespace, 0, len(s.objects))
	for _, o := range s.objects {
		out = append(out, connector.Namespace{
			ID: o, Name: o, Kind: "sobject",
			Metadata: map[string]string{"sobject": o},
		})
	}

	return out, nil
}

// recIterator paginates over SOQL results via the nextRecordsUrl
// pointer.
type recIterator struct {
	s       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	next    string
	started bool
	err     error
	done    bool
}

func (it *recIterator) Next(ctx context.Context) bool {
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

func (it *recIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *recIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *recIterator) Close() error { return nil }

func (it *recIterator) fetchPage(ctx context.Context) bool {
	var path string
	if it.next != "" {
		path = it.next
	} else {
		soql := fmt.Sprintf("SELECT Id, SystemModstamp FROM %s", sanitiseSObject(it.ns.ID))
		if !it.opts.Since.IsZero() {
			soql += " WHERE SystemModstamp >= " + it.opts.Since.UTC().Format(time.RFC3339)
		}
		soql += " ORDER BY SystemModstamp ASC"
		q := url.Values{}
		q.Set("q", soql)
		path = "/query?" + q.Encode()
	}
	resp, err := it.s.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: salesforce: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("salesforce: query status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Done           bool   `json:"done"`
		NextRecordsURL string `json:"nextRecordsUrl"`
		Records        []struct {
			ID             string `json:"Id"`
			SystemModstamp string `json:"SystemModstamp"`
		} `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("salesforce: decode query: %w", err)

		return false
	}
	it.started = true
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, r := range body.Records {
		updated, _ := time.Parse(time.RFC3339, r.SystemModstamp)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          r.ID,
			ETag:        r.SystemModstamp,
			UpdatedAt:   updated,
		})
	}
	if body.Done || body.NextRecordsURL == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		// nextRecordsUrl is rooted ("/services/data/v59.0/query/..."), so
		// strip the version prefix to match our `do` helper.
		it.next = strings.TrimPrefix(body.NextRecordsURL, "/services/data/"+it.s.apiVersion)
	}

	return true
}

// ListDocuments returns a SOQL-paginated iterator.
func (s *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("salesforce: bad connection type")
	}

	return &recIterator{s: s, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches the full record by /sobjects/{type}/{id}.
func (s *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("salesforce: bad connection type")
	}
	resp, err := s.do(ctx, conn, http.MethodGet, "/sobjects/"+sanitiseSObject(ref.NamespaceID)+"/"+ref.ID, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("salesforce: get sobject status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("salesforce: read body: %w", err)
	}

	var doc struct {
		Name           string `json:"Name"`
		SystemModstamp string `json:"SystemModstamp"`
		CreatedDate    string `json:"CreatedDate"`
	}
	_ = json.Unmarshal(body, &doc)
	updated, _ := time.Parse(time.RFC3339, doc.SystemModstamp)
	created, _ := time.Parse(time.RFC3339, doc.CreatedDate)
	title := doc.Name
	if title == "" {
		title = ref.NamespaceID + "/" + ref.ID
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "application/json",
		Title:     title,
		Size:      int64(len(body)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(body))),
		Metadata:  map[string]string{"sobject": ref.NamespaceID},
	}, nil
}

// Subscribe is unsupported — Salesforce uses a Push Topic /
// Platform Events channel that is outside the scope of this
// connector.
func (s *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (s *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync runs a SOQL query with `SystemModstamp > cursor`. The
// cursor is an ISO-8601 timestamp; the new cursor returned is the
// latest SystemModstamp seen. Empty cursor on first call returns
// the current cursor without backfilling.
//
// The cursor is round-tripped through platform-managed storage
// between calls, so before interpolating it into the SOQL WHERE
// clause we parse it as time.RFC3339 and re-format the parsed
// value. A tampered cursor (e.g. an injected SOQL clause stored
// in place of the timestamp) fails to parse and returns
// connector.ErrInvalidConfig, matching the type-level guarantee
// the ListDocuments iterator gets from its time.Time `opts.Since`.
func (s *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("salesforce: bad connection type")
	}
	soql := fmt.Sprintf("SELECT Id, SystemModstamp FROM %s", sanitiseSObject(ns.ID))
	if cursor != "" {
		ts, err := time.Parse(time.RFC3339, cursor)
		if err != nil {
			return nil, "", fmt.Errorf("%w: salesforce: cursor not RFC3339: %v", connector.ErrInvalidConfig, err)
		}
		soql += " WHERE SystemModstamp > " + ts.UTC().Format(time.RFC3339)
	}
	soql += " ORDER BY SystemModstamp ASC"
	q := url.Values{}
	q.Set("q", soql)
	resp, err := s.do(ctx, conn, http.MethodGet, "/query?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Surface 429 as connector.ErrRateLimited so the adaptive rate
	// limiter reacts to throttling during delta sync just like the
	// ListDocuments iterator above does.
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: salesforce: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("salesforce: query status=%d", resp.StatusCode)
	}
	var body struct {
		Records []struct {
			ID             string `json:"Id"`
			SystemModstamp string `json:"SystemModstamp"`
		} `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("salesforce: decode query: %w", err)
	}
	changes := make([]connector.DocumentChange, 0, len(body.Records))
	newCursor := cursor
	for _, r := range body.Records {
		updated, _ := time.Parse(time.RFC3339, r.SystemModstamp)
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          r.ID,
				ETag:        r.SystemModstamp,
				UpdatedAt:   updated,
			},
		})
		if r.SystemModstamp > newCursor {
			newCursor = r.SystemModstamp
		}
	}
	if cursor == "" {
		changes = nil
	}

	return changes, newCursor, nil
}

// do executes an HTTP request rooted at /services/data/<apiVersion>.
func (s *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.instanceURL, "/") + "/services/data/" + s.apiVersion + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("salesforce: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+conn.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("salesforce: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// resolveInstance lets tests override the instance URL while
// production callers continue to pass their real instance.
func (s *Connector) resolveInstance(instanceURL string) string {
	if s.baseURL != "" {
		return s.baseURL
	}

	return instanceURL
}

// sanitiseSObject defends against SOQL injection by stripping any
// character not part of a valid SObject API name. SObject names
// are restricted to letters, digits, and underscores; anything
// else here is necessarily an attacker-controlled value.
func sanitiseSObject(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		}
	}

	return b.String()
}

// Register registers the Salesforce connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
