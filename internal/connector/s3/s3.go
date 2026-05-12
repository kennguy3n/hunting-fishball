// Package s3 implements the S3-compatible object storage
// SourceConnector + DeltaSyncer. Works against AWS S3 plus the
// long tail of S3-compatible stores (MinIO, Backblaze B2,
// Cloudflare R2, Wasabi) — anything that speaks the
// list-objects-v2 + GET-object API and accepts AWS Signature V4.
//
// Credentials must be a JSON blob:
//
//	{
//	  "access_key_id":     "...",   // required
//	  "secret_access_key": "...",   // required
//	  "endpoint":          "...",   // optional, defaults to https://s3.<region>.amazonaws.com
//	  "bucket":            "...",   // required
//	  "region":            "..."    // optional, defaults to us-east-1
//	}
//
// We intentionally use stdlib net/http with an inlined SigV4
// signer (see sigv4.go) rather than aws-sdk-go-v2 so the unit
// tests can run pure-`httptest` without any AWS-specific
// transport/credential plumbing.
package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
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
	Name = "s3"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region,omitempty"`
}

// Connector implements connector.SourceConnector + DeltaSyncer.
type Connector struct {
	httpClient *http.Client
	nowFn      func() time.Time
}

// Option configures a Connector.
type Option func(*Connector)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(s *Connector) { s.httpClient = c } }

// WithNow injects a wall-clock function so SigV4 signatures are
// deterministic under test.
func WithNow(fn func() time.Time) Option { return func(s *Connector) { s.nowFn = fn } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	s := &Connector{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		nowFn:      time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.nowFn == nil {
		s.nowFn = time.Now
	}

	return s
}

// connection holds the per-call state derived from ConnectorConfig.
type connection struct {
	tenantID string
	sourceID string
	creds    Credentials
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
	if c.AccessKeyID == "" {
		return fmt.Errorf("%w: access_key_id required", connector.ErrInvalidConfig)
	}
	if c.SecretAccessKey == "" {
		return fmt.Errorf("%w: secret_access_key required", connector.ErrInvalidConfig)
	}
	if c.Bucket == "" {
		return fmt.Errorf("%w: bucket required", connector.ErrInvalidConfig)
	}

	return nil
}

// Connect issues a HEAD on the bucket as a cheap auth check.
func (s *Connector) Connect(ctx context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	if err := s.Validate(ctx, cfg); err != nil {
		return nil, err
	}
	var creds Credentials
	if err := json.Unmarshal(cfg.Credentials, &creds); err != nil {
		return nil, fmt.Errorf("%w: %v", connector.ErrInvalidConfig, err)
	}
	if creds.Region == "" {
		creds.Region = "us-east-1"
	}

	conn := &connection{tenantID: cfg.TenantID, sourceID: cfg.SourceID, creds: creds}
	resp, err := s.do(ctx, conn, http.MethodHead, "/", nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotImplemented {
		return nil, fmt.Errorf("s3: head bucket failed: status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces returns the bucket as a single namespace. Some
// callers want to scope by prefix instead; we expose that via
// Namespace.ID = "<bucket>/<prefix>" where prefix is optional.
func (s *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("s3: bad connection type")
	}

	return []connector.Namespace{{
		ID:       conn.creds.Bucket,
		Name:     conn.creds.Bucket,
		Kind:     "bucket",
		Metadata: map[string]string{"bucket": conn.creds.Bucket, "region": conn.creds.Region},
	}}, nil
}

// objectListResult mirrors the S3 ListObjectsV2 XML response.
type objectListResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		ETag         string    `xml:"ETag"`
		LastModified time.Time `xml:"LastModified"`
		Size         int64     `xml:"Size"`
	} `xml:"Contents"`
}

// objIterator paginates over ListObjectsV2 results.
type objIterator struct {
	s         *Connector
	conn      *connection
	ns        connector.Namespace
	opts      connector.ListOpts
	page      []connector.DocumentRef
	pageIdx   int
	cursor    string
	startedAt bool
	err       error
	done      bool
}

func (it *objIterator) Next(ctx context.Context) bool {
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

func (it *objIterator) Doc() connector.DocumentRef {
	if it.pageIdx == 0 {
		return connector.DocumentRef{}
	}

	return it.page[it.pageIdx-1]
}

func (it *objIterator) Err() error {
	if it.err == nil && it.done {
		return connector.ErrEndOfPage
	}

	return it.err
}

func (it *objIterator) Close() error { return nil }

// nsPrefix splits a "bucket/prefix" namespace ID into its
// prefix part (or empty for the whole bucket).
func nsPrefix(ns connector.Namespace, bucket string) string {
	if ns.ID == "" || ns.ID == bucket {
		return ""
	}
	if strings.HasPrefix(ns.ID, bucket+"/") {
		return strings.TrimPrefix(ns.ID, bucket+"/")
	}

	return ns.ID
}

func (it *objIterator) fetchPage(ctx context.Context) bool {
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", strconv.Itoa(pageOr(it.opts.PageSize, 1000)))
	if prefix := nsPrefix(it.ns, it.conn.creds.Bucket); prefix != "" {
		q.Set("prefix", prefix)
	}
	if it.cursor != "" {
		q.Set("continuation-token", it.cursor)
	} else if it.opts.PageToken != "" {
		q.Set("continuation-token", it.opts.PageToken)
	}
	// Since-filter: surface via `start-after` (lexicographic key
	// floor; S3 has no native `since-modified` filter in list-v2).
	if !it.opts.Since.IsZero() && !it.startedAt {
		q.Set("start-after", it.opts.Since.UTC().Format(time.RFC3339))
		it.startedAt = true
	}

	resp, err := it.s.do(ctx, it.conn, http.MethodGet, "/?"+q.Encode(), nil, nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	// AWS S3 emits "503 Slow Down" for native throttling while
	// MinIO / R2 / Wasabi proxies often surface "429 Too Many
	// Requests". Treat both as ErrRateLimited so the adaptive
	// limiter can react to either signal.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		it.err = fmt.Errorf("%w: s3: status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("s3: list-objects-v2 status=%d", resp.StatusCode)

		return false
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		it.err = fmt.Errorf("s3: read list body: %w", err)

		return false
	}
	var body objectListResult
	if err := xml.Unmarshal(raw, &body); err != nil {
		it.err = fmt.Errorf("s3: decode list body: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.pageIdx = 0
	for _, obj := range body.Contents {
		// Filter further on Since since list-objects-v2 has no
		// modified-time filter; we asked for start-after as a
		// best-effort floor.
		if !it.opts.Since.IsZero() && obj.LastModified.Before(it.opts.Since) {
			continue
		}
		it.page = append(it.page, connector.DocumentRef{
			NamespaceID: it.ns.ID,
			ID:          obj.Key,
			ETag:        strings.Trim(obj.ETag, "\""),
			UpdatedAt:   obj.LastModified.UTC(),
		})
	}
	if !body.IsTruncated || body.NextContinuationToken == "" {
		it.done = true
		if len(it.page) == 0 {
			return false
		}
	} else {
		it.cursor = body.NextContinuationToken
	}

	return true
}

// ListDocuments returns a paginated iterator over the bucket / prefix.
func (s *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, opts connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("s3: bad connection type")
	}

	return &objIterator{s: s, conn: conn, ns: ns, opts: opts}, nil
}

// FetchDocument downloads a single object.
func (s *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("s3: bad connection type")
	}

	resp, err := s.do(ctx, conn, http.MethodGet, "/"+ref.ID, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("s3: get object %q status=%d", ref.ID, resp.StatusCode)
	}
	updated := ref.UpdatedAt
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := time.Parse(http.TimeFormat, lm); err == nil {
			updated = t.UTC()
		}
	}
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

	return &connector.Document{
		Ref:       ref,
		MIMEType:  resp.Header.Get("Content-Type"),
		Title:     ref.ID,
		Size:      size,
		UpdatedAt: updated,
		Content:   resp.Body,
		Metadata:  map[string]string{"bucket": conn.creds.Bucket, "key": ref.ID},
	}, nil
}

// Subscribe is unsupported — S3 uses event-bridge / SQS push
// rather than a long-poll subscription.
func (s *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (s *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses list-objects-v2 with `start-after` to surface
// objects whose key sorts after the previous cursor. The cursor
// is the highest key seen on the previous call. An empty cursor
// on the first call returns the current cursor without
// backfilling — matching the DeltaSyncer contract for the
// rest of the connectors.
//
// On bootstrap (cursor == "") we paginate through every page
// of the bucket / prefix scope so newCursor is the *true* last
// key, not just the 1000th key of the first page. Without that
// loop, any scope holding more than `max-keys` objects would
// hand back a stale cursor and the next DeltaSync call would
// (incorrectly) re-ingest every key past position 1000 as a
// "new change" — violating the DeltaSyncer contract. S3 has no
// "last key" shortcut and no native modified-time filter on
// list-objects-v2, so a forward pagination walk is the only
// option. Cost is O(N / max-keys) list calls on the first sync,
// then O(1) per steady-state call.
func (s *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("s3: bad connection type")
	}

	body, err := s.listObjectsPage(ctx, conn, ns, cursor, "")
	if err != nil {
		return nil, "", err
	}

	changes := make([]connector.DocumentChange, 0, len(body.Contents))
	newCursor := cursor
	// Initial call (empty cursor): walk every page to find the
	// true last key. Tracking only the last key per page keeps
	// memory O(1) — we never accumulate Contents.
	if cursor == "" {
		if len(body.Contents) > 0 {
			newCursor = body.Contents[len(body.Contents)-1].Key
		}
		token := body.NextContinuationToken
		for body.IsTruncated && token != "" {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
			body, err = s.listObjectsPage(ctx, conn, ns, cursor, token)
			if err != nil {
				return nil, "", err
			}
			if len(body.Contents) > 0 {
				newCursor = body.Contents[len(body.Contents)-1].Key
			}
			token = body.NextContinuationToken
		}

		return changes, newCursor, nil
	}
	for _, obj := range body.Contents {
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref: connector.DocumentRef{
				NamespaceID: ns.ID,
				ID:          obj.Key,
				ETag:        strings.Trim(obj.ETag, "\""),
				UpdatedAt:   obj.LastModified.UTC(),
			},
		})
		newCursor = obj.Key
	}

	return changes, newCursor, nil
}

// listObjectsPage issues a single list-objects-v2 request and
// decodes the XML response. `cursor` is used as `start-after`
// (steady-state filter), `token` is used as
// `continuation-token` (bootstrap pagination). Callers pass
// at most one of the two — when both are set, list-objects-v2
// resumes from the continuation token and silently ignores
// start-after.
func (s *Connector) listObjectsPage(ctx context.Context, conn *connection, ns connector.Namespace, cursor, token string) (objectListResult, error) {
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", "1000")
	if prefix := nsPrefix(ns, conn.creds.Bucket); prefix != "" {
		q.Set("prefix", prefix)
	}
	if token != "" {
		q.Set("continuation-token", token)
	} else if cursor != "" {
		q.Set("start-after", cursor)
	}

	resp, err := s.do(ctx, conn, http.MethodGet, "/?"+q.Encode(), nil, nil)
	if err != nil {
		return objectListResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return objectListResult{}, fmt.Errorf("%w: s3: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return objectListResult{}, fmt.Errorf("s3: list-objects-v2 status=%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return objectListResult{}, fmt.Errorf("s3: read body: %w", err)
	}
	var body objectListResult
	if err := xml.Unmarshal(raw, &body); err != nil {
		return objectListResult{}, fmt.Errorf("s3: decode body: %w", err)
	}

	return body, nil
}

// do runs a SigV4-signed HTTP request against the bucket. path
// should start with "/" and may contain a query string already
// (e.g. "/?list-type=2&…"). The body, when non-nil, is read
// fully into memory so we can compute the SHA-256 required by
// SigV4.
func (s *Connector) do(ctx context.Context, conn *connection, method, path string, body io.Reader, extraHeaders http.Header) (*http.Response, error) {
	endpoint := conn.creds.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", conn.creds.Region)
	}
	target := strings.TrimRight(endpoint, "/") + "/" + conn.creds.Bucket + path
	if !strings.Contains(path, "/") || path[0] != '/' {
		target = strings.TrimRight(endpoint, "/") + "/" + conn.creds.Bucket + "/" + path
	}

	var payload []byte
	var bodyHash string
	if body != nil {
		var err error
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("s3: buffer body: %w", err)
		}
		bodyHash = sha256Hex(payload)
	} else {
		bodyHash = emptyPayload
	}

	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, fmt.Errorf("s3: build request: %w", err)
	}
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	signRequestV4(req, conn.creds.AccessKeyID, conn.creds.SecretAccessKey, conn.creds.Region, bodyHash, s.nowFn())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// pageOr returns v when caller supplied a positive page size and
// otherwise falls back to the connector default.
func pageOr(v, def int) int {
	if v > 0 {
		return v
	}

	return def
}

// Register registers the S3 connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
