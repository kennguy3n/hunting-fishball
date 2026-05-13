// Package zendesk implements the Zendesk Support SourceConnector +
// DeltaSyncer against the Zendesk REST API at
// https://<subdomain>.zendesk.com/api/v2. Authentication uses either
// an API token (basic auth with `<email>/token` + token) or an OAuth
// bearer token; we expose both via a single Credentials struct.
//
// Credentials must be a JSON blob:
//
//	{
//	 "subdomain":    "acme",            // required
//	 "email":        "agent@acme.com",  // required if api_token set
//	 "api_token":    "...",             // either api_token...
//	 "access_token": "..."              // ...or oauth bearer
//	}
//
// The connector exposes a single synthetic namespace "tickets". Delta
// is driven by Zendesk's incremental export endpoint
// `/api/v2/incremental/tickets.json?start_time=<unix>` which returns
// a `next_page` cursor and an `end_time` watermark.
package zendesk

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
const Name = "zendesk"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Subdomain   string `json:"subdomain"`
	Email       string `json:"email"`
	APIToken    string `json:"api_token"`
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

// WithBaseURL pins the Zendesk base URL — used by tests.
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
	tenantID string
	sourceID string
	baseURL  string
	authHdr  string
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
	if creds.Subdomain == "" && o.baseURL == "" {
		return fmt.Errorf("%w: subdomain required", connector.ErrInvalidConfig)
	}
	if creds.APIToken == "" && creds.AccessToken == "" {
		return fmt.Errorf("%w: api_token or access_token required", connector.ErrInvalidConfig)
	}
	if creds.APIToken != "" && creds.Email == "" {
		return fmt.Errorf("%w: email required when api_token is set", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /api/v2/users/me as a cheap auth check.
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
		base = "https://" + creds.Subdomain + ".zendesk.com"
	}
	auth := ""
	if creds.AccessToken != "" {
		auth = "Bearer " + creds.AccessToken
	} else {
		raw := creds.Email + "/token:" + creds.APIToken
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, baseURL: base, authHdr: auth}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v2/users/me.json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: zendesk: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zendesk: users/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the single synthetic namespace "tickets".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "tickets", Name: "Tickets", Kind: "tickets"}}, nil
}

type ticketEntry struct {
	ID        int64  `json:"id"`
	Subject   string `json:"subject"`
	UpdatedAt string `json:"updated_at"`
	CreatedAt string `json:"created_at"`
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
	// Zendesk's `next_page` response field (a full URL). Empty
	// before the first fetch and again once the API signals the
	// final page. Round-22 pagination fix.
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
	// Round-22 pagination fix: follow Zendesk's `next_page` URL
	// across pages instead of stopping after the first response.
	path := "/api/v2/tickets.json"
	if it.nextPath != "" {
		path = it.nextPath
	}
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, path, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: zendesk: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("zendesk: list status=%d", resp.StatusCode)

		return false
	}
	var body struct {
		Tickets  []ticketEntry `json:"tickets"`
		NextPage string        `json:"next_page"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("zendesk: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, t := range body.Tickets {
		ref := connector.DocumentRef{NamespaceID: "tickets", ID: strconv.FormatInt(t.ID, 10)}
		if ts, perr := time.Parse(time.RFC3339, t.UpdatedAt); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.nextPath = ""
	if body.NextPage != "" {
		if u, perr := url.Parse(body.NextPage); perr == nil && u.Path != "" {
			it.nextPath = u.RequestURI()
		}
	}
	it.done = it.nextPath == ""

	return true
}

// ListDocuments enumerates Zendesk tickets.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("zendesk: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads ticket metadata + comments-style body.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("zendesk: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/api/v2/tickets/"+url.PathEscape(ref.ID)+".json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: zendesk: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zendesk: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var body struct {
		Ticket struct {
			ID          int64  `json:"id"`
			Subject     string `json:"subject"`
			Description string `json:"description"`
			UpdatedAt   string `json:"updated_at"`
			CreatedAt   string `json:"created_at"`
		} `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("zendesk: decode ticket: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, body.Ticket.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, body.Ticket.UpdatedAt)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}
	idStr := strconv.FormatInt(body.Ticket.ID, 10)

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "tickets", ID: idStr, UpdatedAt: updated},
		MIMEType:  "text/plain",
		Title:     body.Ticket.Subject,
		Size:      int64(len(body.Ticket.Description)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body.Ticket.Description)),
	}, nil
}

// Subscribe is unsupported — Zendesk surfaces change events via the
// incremental export endpoint exposed through DeltaSync.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls the incremental-tickets export endpoint. The
// cursor is a unix `start_time`. Empty cursor returns "now" as the
// new cursor (per the bootstrap contract) without backfilling.
// Round-23 Devin Review fix: walk every page of the incremental
// export by following the `next_page` URL until Zendesk signals
// `count < 1000` (the documented end-of-stream sentinel). Without
// this loop a tenant with > 1000 ticket updates between syncs
// would silently lose the trailing records until the next cycle.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("zendesk: bad connection type")
	}
	if cursor == "" {
		return nil, strconv.FormatInt(time.Now().UTC().Unix(), 10), nil
	}
	start, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("zendesk: bad cursor %q: %w", cursor, err)
	}
	var changes []connector.DocumentChange
	next := start
	// Zendesk's incremental export returns up to 1000 records per
	// page; `count < 1000` (or `end_of_stream == true`) marks EOF.
	path := "/api/v2/incremental/tickets.json?start_time=" + strconv.FormatInt(start, 10)
	for {
		resp, err := o.do(ctx, conn, http.MethodGet, path, nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		retryAfter := resp.Header.Get("Retry-After")
		var body struct {
			Tickets     []ticketEntry `json:"tickets"`
			EndTime     int64         `json:"end_time"`
			Count       int           `json:"count"`
			NextPage    string        `json:"next_page"`
			EndOfStream bool          `json:"end_of_stream"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			retry := parseRetryAfter(retryAfter)
			return nil, "", fmt.Errorf("%w: zendesk: status=%d retry_after=%s", connector.ErrRateLimited, status, retry)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("zendesk: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("zendesk: decode delta: %w", decodeErr)
		}
		for _, t := range body.Tickets {
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: strconv.FormatInt(t.ID, 10)}
			if ts, perr := time.Parse(time.RFC3339, t.UpdatedAt); perr == nil {
				ref.UpdatedAt = ts.UTC()
			}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
		}
		if body.EndTime > 0 {
			next = body.EndTime
		}
		// Three EOF signals; any one ends the walk.
		if body.EndOfStream || body.Count < 1000 || body.NextPage == "" {
			break
		}
		u, perr := url.Parse(body.NextPage)
		if perr != nil || u.Path == "" {
			break
		}
		path = u.RequestURI()
	}

	return changes, strconv.FormatInt(next, 10), nil
}

// parseRetryAfter returns a debug-friendly representation of the
// Retry-After header. The caller only logs/wraps the value; we
// don't sleep here because the adaptive rate limiter does.
func parseRetryAfter(v string) string {
	if v == "" {
		return ""
	}
	if _, err := strconv.Atoi(v); err == nil {
		return v + "s"
	}

	return v
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("zendesk: build request: %w", err)
	}
	req.Header.Set("Authorization", conn.authHdr)
	req.Header.Set("Accept", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zendesk: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
