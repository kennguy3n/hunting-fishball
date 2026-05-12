// Package linear implements the Linear SourceConnector +
// DeltaSyncer + WebhookReceiver. Linear exposes a single GraphQL
// endpoint at https://api.linear.app/graphql; we send POST
// requests with an `Authorization: <api_key>` header (Linear's
// docs note that the personal API key is NOT prefixed with
// "Bearer ") and parse the JSON response.
//
// Credentials must be a JSON blob:
//
//	{
//	  "api_key":         "lin_api_...",  // required
//	  "signing_secret":  "..."            // optional, used by VerifyWebhookRequest
//	}
package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

const (
	// Name is the registry-visible connector name.
	Name = "linear"

	defaultBaseURL = "https://api.linear.app/graphql"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	APIKey        string `json:"api_key"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer +
// WebhookReceiver.
type Connector struct {
	httpClient    *http.Client
	baseURL       string
	signingSecret string
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(l *Connector) { l.httpClient = c } }

// WithBaseURL overrides the Linear GraphQL endpoint — used by tests.
func WithBaseURL(u string) Option { return func(l *Connector) { l.baseURL = u } }

// WithSigningSecret enables Linear webhook signature verification.
// Empty disables verification (development default).
func WithSigningSecret(s string) Option { return func(l *Connector) { l.signingSecret = s } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	l := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultBaseURL,
	}
	for _, opt := range opts {
		opt(l)
	}

	return l
}

type connection struct {
	tenantID string
	sourceID string
	apiKey   string
	userID   string
}

func (c *connection) TenantID() string { return c.tenantID }
func (c *connection) SourceID() string { return c.sourceID }

// Validate parses and sanity-checks the credential blob.
func (l *Connector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
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
	if c.APIKey == "" {
		return fmt.Errorf("%w: api_key required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect runs a `viewer { id }` GraphQL query as the auth check.
func (l *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := l.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, apiKey: creds.APIKey}

	var out struct {
		Data struct {
			Viewer struct {
				ID string `json:"id"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := l.query(ctx, conn, `query { viewer { id } }`, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("linear: viewer query failed: %s", out.Errors[0].Message)
	}
	conn.userID = out.Data.Viewer.ID

	return conn, nil
}

// ListNamespaces returns teams as namespaces (Linear's coarsest
// scoping unit above the workspace).
func (l *Connector) ListNamespaces(ctx context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("linear: bad connection type")
	}

	var out struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID   string `json:"id"`
					Key  string `json:"key"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
	}
	if err := l.query(ctx, conn, `query { teams { nodes { id key name } } }`, nil, &out); err != nil {
		return nil, err
	}
	ns := make([]connector.Namespace, 0, len(out.Data.Teams.Nodes))
	for _, t := range out.Data.Teams.Nodes {
		ns = append(ns, connector.Namespace{
			ID: t.ID, Name: t.Name, Kind: "team",
			Metadata: map[string]string{"team_key": t.Key},
		})
	}

	return ns, nil
}

// issueIterator paginates over `issues` connection on a team.
type issueIterator struct {
	l       *Connector
	conn    *connection
	ns      connector.Namespace
	opts    connector.ListOpts
	page    []connector.DocumentRef
	pageIdx int
	cursor  string
	err     error
	done    bool
}

func (it *issueIterator) Next(ctx context.Context) bool {
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

func (it *issueIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *issueIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *issueIterator) Close() error { return nil }

func (it *issueIterator) fetchPage(ctx context.Context) bool {
	first := pageOr(it.opts.PageSize, 50)
	vars := map[string]any{
		"teamId": it.ns.ID,
		"first":  first,
	}
	if it.cursor != "" {
		vars["after"] = it.cursor
	} else if it.opts.PageToken != "" {
		vars["after"] = it.opts.PageToken
	}
	filter := map[string]any{}
	if !it.opts.Since.IsZero() {
		filter["updatedAt"] = map[string]any{"gte": it.opts.Since.UTC().Format(time.RFC3339)}
	}
	vars["filter"] = filter

	q := `query($teamId: String!, $first: Int!, $after: String, $filter: IssueFilter) {
		team(id: $teamId) {
			issues(first: $first, after: $after, filter: $filter) {
				pageInfo { hasNextPage endCursor }
				nodes { id identifier updatedAt }
			}
		}
	}`
	var out struct {
		Data struct {
			Team struct {
				Issues struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID         string `json:"id"`
						Identifier string `json:"identifier"`
						UpdatedAt  string `json:"updatedAt"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"team"`
		} `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := it.l.query(ctx, it.conn, q, vars, &out); err != nil {
		it.err = err

		return false
	}
	if len(out.Errors) > 0 {
		it.err = fmt.Errorf("linear: issues query failed: %s", out.Errors[0].Message)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, n := range out.Data.Team.Issues.Nodes {
		updated, _ := time.Parse(time.RFC3339, n.UpdatedAt)
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          n.ID,
			ETag:        n.UpdatedAt,
			UpdatedAt:   updated,
		})
	}
	if !out.Data.Team.Issues.PageInfo.HasNextPage {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = out.Data.Team.Issues.PageInfo.EndCursor
	}

	return true
}

// ListDocuments enumerates issues in the team.
func (l *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("linear: bad connection type")
	}

	return &issueIterator{l: l, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument fetches a single issue by ID.
func (l *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("linear: bad connection type")
	}

	q := `query($id: String!) {
		issue(id: $id) {
			id identifier title description createdAt updatedAt
			creator { name email }
		}
	}`
	var out struct {
		Data struct {
			Issue struct {
				ID          string `json:"id"`
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				CreatedAt   string `json:"createdAt"`
				UpdatedAt   string `json:"updatedAt"`
				Creator     struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"creator"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := l.query(ctx, conn, q, map[string]any{"id": ref.ID}, &out); err != nil {
		return nil, err
	}
	issue := out.Data.Issue
	if issue.ID == "" {
		return nil, fmt.Errorf("linear: issue %s not found", ref.ID)
	}
	created, _ := time.Parse(time.RFC3339, issue.CreatedAt)
	updated, _ := time.Parse(time.RFC3339, issue.UpdatedAt)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  "text/markdown",
		Title:     issue.Title,
		Author:    issue.Creator.Email,
		Size:      int64(len(issue.Description) + len(issue.Title)),
		CreatedAt: created,
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(issue.Description)),
		Metadata:  map[string]string{"identifier": issue.Identifier},
	}, nil
}

// Subscribe is unsupported — Linear pushes changes via the
// HandleWebhook path on WebhookReceiver.
func (l *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (l *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync polls for issues `updatedAt > cursor`. The cursor is
// an ISO-8601 timestamp; the new cursor returned is the latest
// `updatedAt` value seen on this call. Empty cursor on the first
// call returns the current cursor without backfilling — matching
// the DeltaSyncer contract.
//
// To honor the no-backfill contract we must bootstrap with the
// single most-recently-updated issue (DESCENDING sort + first:1)
// so newCursor represents "now". Linear's `orderBy: updatedAt`
// defaults to ascending; `first: 50` on the first call would
// return the 50 oldest-updated issues and newCursor would be the
// 50th-oldest timestamp — possibly months stale, causing the next
// call's `updatedAt > <stale>` filter to backfill nearly the
// entire team's history as "changes".
//
// Subsequent calls (cursor != "") use ascending so the page
// starts with the oldest unseen change, giving stable forward-only
// cursor advancement under the GT filter.
func (l *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("linear: bad connection type")
	}
	var (
		q    string
		vars map[string]any
	)
	if cursor == "" {
		q = `query($teamId: String!) {
			team(id: $teamId) {
				issues(first: 1, orderBy: updatedAt, sort: [{updatedAt: {order: Descending}}]) {
					nodes { id identifier updatedAt archivedAt }
				}
			}
		}`
		vars = map[string]any{"teamId": ns.ID}
	} else {
		q = `query($teamId: String!, $filter: IssueFilter) {
			team(id: $teamId) {
				issues(first: 50, filter: $filter, orderBy: updatedAt) {
					nodes { id identifier updatedAt archivedAt }
				}
			}
		}`
		vars = map[string]any{
			"teamId": ns.ID,
			"filter": map[string]any{"updatedAt": map[string]any{"gt": cursor}},
		}
	}
	var out struct {
		Data struct {
			Team struct {
				Issues struct {
					Nodes []struct {
						ID         string `json:"id"`
						Identifier string `json:"identifier"`
						UpdatedAt  string `json:"updatedAt"`
						ArchivedAt string `json:"archivedAt"`
					} `json:"nodes"`
				} `json:"issues"`
			} `json:"team"`
		} `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := l.query(ctx, conn, q, vars, &out); err != nil {
		return nil, "", err
	}
	if len(out.Errors) > 0 {
		return nil, "", fmt.Errorf("linear: delta issues query failed: %s", out.Errors[0].Message)
	}

	changes := make([]connector.DocumentChange, 0, len(out.Data.Team.Issues.Nodes))
	newCursor := cursor
	for _, n := range out.Data.Team.Issues.Nodes {
		updated, _ := time.Parse(time.RFC3339, n.UpdatedAt)
		kind := connector.ChangeUpserted
		if n.ArchivedAt != "" {
			kind = connector.ChangeDeleted
		}
		changes = append(changes, connector.DocumentChange{
			Kind: kind,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          n.ID,
				ETag:        n.UpdatedAt,
				UpdatedAt:   updated,
			},
		})
		if n.UpdatedAt > newCursor {
			newCursor = n.UpdatedAt
		}
	}

	// Initial sync (empty cursor): only return the current cursor.
	if cursor == "" {
		changes = nil
	}

	return changes, newCursor, nil
}

// HandleWebhook decodes a Linear webhook payload into a list of
// DocumentChange values. Linear's payload shape is
//
//	{
//	  "action":   "create" | "update" | "remove",
//	  "type":     "Issue" | ...,
//	  "data":     {"id": "...", "updatedAt": "...", "team": {"id": "..."}}
//	}
func (l *Connector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	if len(payload) == 0 {
		return nil, errors.New("linear: empty webhook payload")
	}
	var env struct {
		Action string `json:"action"`
		Type   string `json:"type"`
		Data   struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updatedAt"`
			Team      struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("linear: decode webhook: %w", err)
	}
	if env.Type != "Issue" || env.Data.ID == "" {
		return nil, nil
	}
	updated, _ := time.Parse(time.RFC3339, env.Data.UpdatedAt)
	kind := connector.ChangeUpserted
	if env.Action == "remove" {
		kind = connector.ChangeDeleted
	}

	return []connector.DocumentChange{{
		Kind: kind,
		Ref: connector.DocumentRef{
			NamespaceID: env.Data.Team.ID,
			ID:          env.Data.ID,
			ETag:        env.Data.UpdatedAt,
			UpdatedAt:   updated,
		},
	}}, nil
}

// WebhookPath implements connector.WebhookReceiver.
func (l *Connector) WebhookPath() string { return "/linear" }

// VerifyWebhookRequest validates Linear's HMAC-SHA256 signature
// header (`Linear-Signature`). When signingSecret is empty
// verification is skipped (development default).
func (l *Connector) VerifyWebhookRequest(headers map[string][]string, payload []byte) error {
	if l.signingSecret == "" {
		return nil
	}
	sig := strings.TrimSpace(connector.FirstHeader(headers, "Linear-Signature"))
	if sig == "" {
		return connector.ErrWebhookSignatureMissing
	}
	mac := hmac.New(sha256.New, []byte(l.signingSecret))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return connector.ErrWebhookSignatureInvalid
	}

	return nil
}

// query runs a GraphQL query against the Linear endpoint and
// decodes the response into out.
func (l *Connector) query(ctx context.Context, conn *connection, q string, vars map[string]any, out any) error {
	body := map[string]any{"query": q}
	if len(vars) > 0 {
		body["variables"] = vars
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("linear: encode query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("linear: build request: %w", err)
	}
	// Linear personal API keys are NOT prefixed with "Bearer ".
	// OAuth tokens use "Bearer "; we accept either by checking
	// the key prefix.
	auth := conn.apiKey
	if strings.HasPrefix(auth, "lin_oauth_") {
		auth = "Bearer " + auth
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("linear: POST %s: %w", l.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: linear: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear: status=%d", resp.StatusCode)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("linear: read response: %w", err)
	}
	// Linear returns 200-OK with errors[0].extensions.code = "RATELIMITED"
	// when the per-key hourly bucket is exhausted (docs/runbooks/linear.md §3).
	// Surface that as ErrRateLimited so adaptive_rate.go can react, same as 429.
	var probe struct {
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if jerr := json.Unmarshal(respBody, &probe); jerr == nil && len(probe.Errors) > 0 {
		if strings.EqualFold(probe.Errors[0].Extensions.Code, "RATELIMITED") {
			return fmt.Errorf("%w: linear: graphql code=%s message=%q", connector.ErrRateLimited, probe.Errors[0].Extensions.Code, probe.Errors[0].Message)
		}
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("linear: decode response: %w", err)
	}

	return nil
}

func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the Linear connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
