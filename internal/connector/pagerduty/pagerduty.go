// Package pagerduty implements the PagerDuty SourceConnector +
// DeltaSyncer against the PagerDuty REST API v2 at
// https://api.pagerduty.com. Authentication uses an API token via
// the `Authorization: Token token=<key>` header.
//
// Credentials must be a JSON blob:
//
//	{
//	 "api_key": "..." // required
//	}
//
// The connector exposes a single synthetic namespace "incidents".
// Delta is driven by `/incidents?since=<ISO8601>&until=<ISO8601>`
// paginated with the offset / limit query parameters.
package pagerduty

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

// Name is the registry-visible connector name.
const Name = "pagerduty"

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIKey string `json:"api_key"`
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

// WithBaseURL pins the PagerDuty base URL — used by tests.
func WithBaseURL(u string) Option { return func(o *Connector) { o.baseURL = u } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "https://api.pagerduty.com",
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID string
	sourceID string
	baseURL  string
	apiKey   string
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
	if creds.APIKey == "" {
		return fmt.Errorf("%w: api_key required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues GET /users/me as a cheap auth check.
func (o *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := o.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{
		tenantID: cfg.TenantID,
		sourceID: cfg.SourceID,
		baseURL:  o.baseURL,
		apiKey:   creds.APIKey,
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: pagerduty: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	// PagerDuty's users/me requires an API token tied to a user — service
	// tokens hit /abilities instead. We accept either as a successful
	// auth check.
	if resp.StatusCode == http.StatusNotFound {
		resp2, err := o.do(ctx, conn, http.MethodGet, "/abilities", nil)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp2.Body.Close() }()
		if resp2.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: pagerduty: status=%d", connector.ErrRateLimited, resp2.StatusCode)
		}
		if resp2.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("pagerduty: abilities status=%d", resp2.StatusCode)
		}

		return conn, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pagerduty: users/me status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the single synthetic namespace "incidents".
func (o *Connector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return []connector.Namespace{{ID: "incidents", Name: "Incidents", Kind: "incidents"}}, nil
}

type incidentEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"last_status_change_at"`
}

type listEnvelope struct {
	Incidents []incidentEntry `json:"incidents"`
	More      bool            `json:"more"`
	Offset    int             `json:"offset"`
	Limit     int             `json:"limit"`
}

type docIterator struct {
	o      *Connector
	conn   *connection
	ns     connector.Namespace
	page   []connector.DocumentRef
	idx    int
	done   bool
	err    error
	offset int
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
	const perPage = 100
	q := url.Values{}
	q.Set("limit", strconv.Itoa(perPage))
	q.Set("offset", strconv.Itoa(it.offset))
	q.Set("statuses[]", "resolved")
	q.Add("statuses[]", "triggered")
	q.Add("statuses[]", "acknowledged")
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/incidents?"+q.Encode(), nil)
	if err != nil {
		it.err = err

		return false
	}
	status := resp.StatusCode
	var env listEnvelope
	decodeErr := json.NewDecoder(resp.Body).Decode(&env)
	_ = resp.Body.Close()
	if status == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: pagerduty: list status=%d", connector.ErrRateLimited, status)

		return false
	}
	if status != http.StatusOK {
		it.err = fmt.Errorf("pagerduty: list status=%d", status)

		return false
	}
	if decodeErr != nil {
		it.err = fmt.Errorf("pagerduty: decode list: %w", decodeErr)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, e := range env.Incidents {
		ref := connector.DocumentRef{NamespaceID: "incidents", ID: e.ID}
		if ts, perr := time.Parse(time.RFC3339, e.UpdatedAt); perr == nil {
			ref.UpdatedAt = ts.UTC()
		}
		it.page = append(it.page, ref)
	}
	if !env.More || len(env.Incidents) == 0 {
		it.done = true
	} else {
		it.offset += len(env.Incidents)
	}

	return true
}

// ListDocuments enumerates PagerDuty incidents.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("pagerduty: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument loads a PagerDuty incident by ID.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("pagerduty: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/incidents/"+url.PathEscape(ref.ID), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: pagerduty: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pagerduty: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	var env struct {
		Incident incidentEntry `json:"incident"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("pagerduty: decode incident: %w", err)
	}
	created, _ := time.Parse(time.RFC3339, env.Incident.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, env.Incident.UpdatedAt)
	if updated.IsZero() {
		updated = ref.UpdatedAt
	}
	body := env.Incident.Summary
	if body == "" {
		body = env.Incident.Title
	}

	return &connector.Document{
		Ref:       connector.DocumentRef{NamespaceID: "incidents", ID: env.Incident.ID, UpdatedAt: updated},
		MIMEType:  "text/plain",
		Title:     env.Incident.Title,
		Size:      int64(len(body)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(body)),
	}, nil
}

// Subscribe is unsupported — PagerDuty exposes webhooks but not a
// streaming SDK; webhook integration ships separately in the
// platform-backend bridge.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls /incidents filtered by since=<cursor>. Empty
// cursor returns "now" without backfilling.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("pagerduty: bad connection type")
	}
	if cursor == "" {
		return nil, time.Now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("pagerduty: bad cursor %q: %w", cursor, err)
	}
	const perPage = 100
	newest := cursorTime
	var changes []connector.DocumentChange
	offset := 0
	for {
		q := url.Values{}
		q.Set("since", cursor)
		q.Set("until", time.Now().UTC().Format(time.RFC3339))
		q.Set("limit", strconv.Itoa(perPage))
		q.Set("offset", strconv.Itoa(offset))
		q.Add("statuses[]", "resolved")
		q.Add("statuses[]", "triggered")
		q.Add("statuses[]", "acknowledged")
		resp, err := o.do(ctx, conn, http.MethodGet, "/incidents?"+q.Encode(), nil)
		if err != nil {
			return nil, "", err
		}
		status := resp.StatusCode
		var env listEnvelope
		decodeErr := json.NewDecoder(resp.Body).Decode(&env)
		_ = resp.Body.Close()
		if status == http.StatusTooManyRequests {
			return nil, "", fmt.Errorf("%w: pagerduty: status=%d", connector.ErrRateLimited, status)
		}
		if status != http.StatusOK {
			return nil, cursor, fmt.Errorf("pagerduty: delta status=%d", status)
		}
		if decodeErr != nil {
			return nil, "", fmt.Errorf("pagerduty: decode delta: %w", decodeErr)
		}
		for _, e := range env.Incidents {
			ts, perr := time.Parse(time.RFC3339, e.UpdatedAt)
			if perr != nil {
				continue
			}
			ts = ts.UTC()
			ref := connector.DocumentRef{NamespaceID: ns.ID, ID: e.ID, UpdatedAt: ts}
			changes = append(changes, connector.DocumentChange{Kind: connector.ChangeUpserted, Ref: ref})
			if ts.After(newest) {
				newest = ts
			}
		}
		if !env.More || len(env.Incidents) == 0 {
			break
		}
		offset += len(env.Incidents)
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

func (o *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("pagerduty: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token token="+conn.apiKey)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pagerduty: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
