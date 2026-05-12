// Package azureblob implements the Azure Blob Storage
// SourceConnector + DeltaSyncer against the Azure REST API at
// https://<account>.blob.core.windows.net/<container>. We avoid
// the vendor SDK and use stdlib net/http with either an
// account-key (HMAC-SHA256 over the Storage canonical request)
// or a SAS-token (?sv=…&sig=…) appended directly to the URL.
//
// Credentials must be a JSON blob:
//
//	{
//	  "account":   "...",        // required
//	  "container": "...",        // required
//	  "endpoint":  "https://…",  // optional (defaults to blob.core.windows.net)
//	  "sas_token": "?sv=…&sig=…", // optional
//	  "shared_key":"base64..."   // optional (HMAC-SHA256)
//	}
//
// Exactly one of `sas_token` OR `shared_key` must be supplied.
// Delta is implemented via the blob `Last-Modified` header on the
// `?comp=list&restype=container` response — we surface every blob
// whose Last-Modified is strictly greater than the cursor.
package azureblob

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

const (
	// Name is the registry-visible connector name.
	Name = "azure_blob"

	defaultEndpoint = "blob.core.windows.net"
)

// Credentials is the JSON shape Validate / Connect expects.
type Credentials struct {
	Account   string `json:"account"`
	Container string `json:"container"`
	Endpoint  string `json:"endpoint,omitempty"`
	SASToken  string `json:"sas_token,omitempty"`
	SharedKey string `json:"shared_key,omitempty"`
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

// WithNow injects a deterministic clock — used by tests so the
// `x-ms-date` header is reproducible.
func WithNow(now func() time.Time) Option { return func(o *Connector) { o.now = now } }

// New constructs a Connector.
func New(opts ...Option) *Connector {
	o := &Connector{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		now:        func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(o)
	}

	return o
}

type connection struct {
	tenantID  string
	sourceID  string
	baseURL   string
	account   string
	container string
	sasToken  string
	sharedKey []byte
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
	if creds.Account == "" {
		return fmt.Errorf("%w: account required", connector.ErrInvalidConfig)
	}
	if creds.Container == "" {
		return fmt.Errorf("%w: container required", connector.ErrInvalidConfig)
	}
	if creds.SASToken == "" && creds.SharedKey == "" {
		return fmt.Errorf("%w: sas_token or shared_key required", connector.ErrInvalidConfig)
	}
	if creds.SharedKey != "" {
		if _, err := base64.StdEncoding.DecodeString(creds.SharedKey); err != nil {
			return fmt.Errorf("%w: shared_key not base64: %v", connector.ErrInvalidConfig, err)
		}
	}

	return nil
}

// Connect issues an HTTP HEAD on the container to verify auth.
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
		endpoint := creds.Endpoint
		if endpoint == "" {
			endpoint = "https://" + creds.Account + "." + defaultEndpoint
		}
		base = endpoint
	}
	conn := &connection{
		tenantID:  cfg.TenantID,
		sourceID:  cfg.SourceID,
		baseURL:   base,
		account:   creds.Account,
		container: creds.Container,
		sasToken:  creds.SASToken,
	}
	if creds.SharedKey != "" {
		k, _ := base64.StdEncoding.DecodeString(creds.SharedKey)
		conn.sharedKey = k
	}
	resp, err := o.do(ctx, conn, http.MethodHead, "/"+url.PathEscape(conn.container), "restype=container", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: azure_blob: status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("azure_blob: HEAD container status=%d", resp.StatusCode)
	}

	return conn, nil
}

// ListNamespaces exposes the configured container as a single
// namespace. Azure Blob does not have a folder construct on the
// API side; virtual prefixes live in blob names.
func (o *Connector) ListNamespaces(_ context.Context, c connector.Connection) ([]connector.Namespace, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("azure_blob: bad connection type")
	}

	return []connector.Namespace{{ID: conn.container, Name: conn.container, Kind: "container"}}, nil
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

type listBlobsResult struct {
	XMLName xml.Name `xml:"EnumerationResults"`
	Blobs   struct {
		Blob []struct {
			Name       string `xml:"Name"`
			Properties struct {
				LastModified string `xml:"Last-Modified"`
				ETag         string `xml:"Etag"`
				ContentType  string `xml:"Content-Type"`
				ContentLen   int64  `xml:"Content-Length"`
			} `xml:"Properties"`
		} `xml:"Blob"`
	} `xml:"Blobs"`
}

func (it *docIterator) fetch(ctx context.Context) bool {
	resp, err := it.o.do(ctx, it.conn, http.MethodGet, "/"+url.PathEscape(it.conn.container), "comp=list&restype=container", nil)
	if err != nil {
		it.err = err

		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		it.err = fmt.Errorf("%w: azure_blob: list status=%d", connector.ErrRateLimited, resp.StatusCode)

		return false
	}
	if resp.StatusCode != http.StatusOK {
		it.err = fmt.Errorf("azure_blob: list status=%d", resp.StatusCode)

		return false
	}
	var body listBlobsResult
	if err := xml.NewDecoder(resp.Body).Decode(&body); err != nil {
		it.err = fmt.Errorf("azure_blob: decode list: %w", err)

		return false
	}
	it.page = it.page[:0]
	it.idx = 0
	for _, b := range body.Blobs.Blob {
		ref := connector.DocumentRef{NamespaceID: it.ns.ID, ID: b.Name, ETag: b.Properties.ETag}
		if t, perr := http.ParseTime(b.Properties.LastModified); perr == nil {
			ref.UpdatedAt = t.UTC()
		}
		it.page = append(it.page, ref)
	}
	it.done = true

	return true
}

// ListDocuments enumerates blobs in the container.
func (o *Connector) ListDocuments(_ context.Context, c connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("azure_blob: bad connection type")
	}

	return &docIterator{o: o, conn: conn, ns: ns}, nil
}

// FetchDocument GETs the blob bytes and surfaces them as
// Document.Content.
func (o *Connector) FetchDocument(ctx context.Context, c connector.Connection, ref connector.DocumentRef) (*connector.Document, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, errors.New("azure_blob: bad connection type")
	}
	resp, err := o.do(ctx, conn, http.MethodGet, "/"+url.PathEscape(conn.container)+"/"+url.PathEscape(ref.ID), "", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("%w: azure_blob: fetch status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("azure_blob: fetch %s status=%d", ref.ID, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("azure_blob: read body: %w", err)
	}
	updated := ref.UpdatedAt
	if t, perr := http.ParseTime(resp.Header.Get("Last-Modified")); perr == nil {
		updated = t.UTC()
	}

	return &connector.Document{
		Ref:       ref,
		MIMEType:  resp.Header.Get("Content-Type"),
		Title:     ref.ID,
		Size:      int64(len(raw)),
		UpdatedAt: updated,
		Content:   io.NopCloser(strings.NewReader(string(raw))),
		Metadata:  map[string]string{"etag": resp.Header.Get("ETag")},
	}, nil
}

// Subscribe is unsupported — Azure Blob change events live in
// Event Grid which the platform doesn't yet mount.
func (o *Connector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

// Disconnect is a no-op.
func (o *Connector) Disconnect(_ context.Context, _ connector.Connection) error { return nil }

// DeltaSync uses the blob `Last-Modified` header (returned by
// `?comp=list&restype=container`) to filter blobs strictly newer
// than the cursor. The empty-cursor bootstrap returns "now"
// without a backfill.
func (o *Connector) DeltaSync(ctx context.Context, c connector.Connection, ns connector.Namespace, cursor string) ([]connector.DocumentChange, string, error) {
	conn, ok := c.(*connection)
	if !ok {
		return nil, "", errors.New("azure_blob: bad connection type")
	}
	if cursor == "" {
		return nil, o.now().UTC().Format(time.RFC3339), nil
	}
	cursorTime, err := time.Parse(time.RFC3339, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("azure_blob: bad cursor %q: %w", cursor, err)
	}
	req := func() (*http.Request, error) {
		path := "/" + url.PathEscape(conn.container)
		raw := "comp=list&restype=container"
		full := strings.TrimRight(conn.baseURL, "/") + path + "?" + raw
		if conn.sasToken != "" {
			full += "&" + strings.TrimPrefix(conn.sasToken, "?")
		}

		return http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	}
	r, err := req()
	if err != nil {
		return nil, "", fmt.Errorf("azure_blob: build delta: %w", err)
	}
	r.Header.Set("If-Modified-Since", cursorTime.UTC().Format(http.TimeFormat))
	r.Header.Set("x-ms-version", "2021-04-10")
	if conn.sasToken == "" {
		o.signRequest(r, conn)
	}
	resp, err := o.httpClient.Do(r)
	if err != nil {
		return nil, "", fmt.Errorf("azure_blob: delta: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, "", fmt.Errorf("%w: azure_blob: delta status=%d", connector.ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotModified {
		return nil, cursor, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, cursor, fmt.Errorf("azure_blob: delta status=%d", resp.StatusCode)
	}
	var body listBlobsResult
	if err := xml.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("azure_blob: decode delta: %w", err)
	}
	newest := cursorTime
	var changes []connector.DocumentChange
	for _, b := range body.Blobs.Blob {
		t, perr := http.ParseTime(b.Properties.LastModified)
		if perr != nil {
			continue
		}
		t = t.UTC()
		if !t.After(cursorTime) {
			continue
		}
		changes = append(changes, connector.DocumentChange{
			Kind: connector.ChangeUpserted,
			Ref:  connector.DocumentRef{NamespaceID: ns.ID, ID: b.Name, ETag: b.Properties.ETag, UpdatedAt: t},
		})
		if t.After(newest) {
			newest = t
		}
	}

	return changes, newest.UTC().Format(time.RFC3339), nil
}

// do builds, signs (if shared_key), and sends an HTTP request to
// the storage endpoint. rawQuery is the query string sans `?`.
func (o *Connector) do(ctx context.Context, conn *connection, method, path, rawQuery string, body io.Reader) (*http.Response, error) {
	target := strings.TrimRight(conn.baseURL, "/") + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	if conn.sasToken != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + strings.TrimPrefix(conn.sasToken, "?")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("azure_blob: build request: %w", err)
	}
	req.Header.Set("x-ms-version", "2021-04-10")
	if conn.sasToken == "" {
		o.signRequest(req, conn)
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure_blob: %s %s: %w", method, target, err)
	}

	return resp, nil
}

// signRequest applies the Azure Storage shared-key HMAC-SHA256
// signature to req. The canonical request is documented at
// https://learn.microsoft.com/rest/api/storageservices/authorize-with-shared-key
// — we implement the SharedKey (not SharedKeyLite) variant.
func (o *Connector) signRequest(req *http.Request, conn *connection) {
	if len(conn.sharedKey) == 0 {
		return
	}
	date := o.now().UTC().Format(http.TimeFormat)
	req.Header.Set("x-ms-date", date)
	canonHeaders := canonicalizedHeaders(req.Header)
	canonResource := canonicalizedResource(conn.account, req.URL)
	stringToSign := strings.Join([]string{
		req.Method,
		req.Header.Get("Content-Encoding"),
		req.Header.Get("Content-Language"),
		"",
		req.Header.Get("Content-MD5"),
		req.Header.Get("Content-Type"),
		"",
		req.Header.Get("If-Modified-Since"),
		req.Header.Get("If-Match"),
		req.Header.Get("If-None-Match"),
		req.Header.Get("If-Unmodified-Since"),
		req.Header.Get("Range"),
		canonHeaders,
		canonResource,
	}, "\n")
	mac := hmac.New(sha256.New, conn.sharedKey)
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "SharedKey "+conn.account+":"+sig)
}

func canonicalizedHeaders(h http.Header) string {
	keys := make([]string, 0, len(h))
	for k := range h {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "x-ms-") {
			keys = append(keys, lower)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(h.Get(k)))
	}

	return b.String()
}

func canonicalizedResource(account string, u *url.URL) string {
	var b strings.Builder
	b.WriteByte('/')
	b.WriteString(account)
	if u.Path == "" {
		b.WriteByte('/')
	} else {
		b.WriteString(u.Path)
	}
	if q := u.Query(); len(q) > 0 {
		lowerQ := make(url.Values, len(q))
		for k, v := range q {
			lk := strings.ToLower(k)
			lowerQ[lk] = append(lowerQ[lk], v...)
		}
		keys := make([]string, 0, len(lowerQ))
		for k := range lowerQ {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			vals := append([]string(nil), lowerQ[k]...)
			sort.Strings(vals)
			b.WriteByte('\n')
			b.WriteString(k)
			b.WriteByte(':')
			b.WriteString(strings.Join(vals, ","))
		}
	}

	return b.String()
}

// Register registers the connector with the global registry.
func Register() {
	_ = connector.RegisterSourceConnector(Name, func() connector.SourceConnector { return New() })
}

func init() { Register() }
