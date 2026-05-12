package s3_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/s3"
)

func validCreds(t *testing.T, endpoint string) []byte {
	t.Helper()
	b, err := json.Marshal(s3.Credentials{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
		Endpoint:        endpoint,
		Bucket:          "test-bucket",
		Region:          "us-east-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return b
}

func TestS3_Validate(t *testing.T) {
	t.Parallel()

	c := s3.New()
	good, _ := json.Marshal(s3.Credentials{AccessKeyID: "a", SecretAccessKey: "b", Bucket: "x"})
	for _, tc := range []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: good}, true},
		{"missing tenant", connector.ConnectorConfig{SourceID: "s", Credentials: good}, false},
		{"missing source", connector.ConnectorConfig{TenantID: "t", Credentials: good}, false},
		{"empty creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"bad json", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte("nope")}, false},
		{"missing access key", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"secret_access_key":"x","bucket":"b"}`)}, false},
		{"missing secret", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_key_id":"x","bucket":"b"}`)}, false},
		{"missing bucket", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{"access_key_id":"x","secret_access_key":"y"}`)}, false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("expected error")
				}
				if !errors.Is(err, connector.ErrInvalidConfig) {
					t.Fatalf("expected ErrInvalidConfig, got %v", err)
				}
			}
		})
	}
}

func TestS3_Connect_Authenticated(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			http.Error(w, "no sig", http.StatusForbidden)

			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" {
		t.Fatalf("tenant: %q", conn.TenantID())
	}
}

func TestS3_Connect_AuthFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	if _, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestS3_ListNamespaces(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	ns, err := c.ListNamespaces(context.Background(), conn)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 1 || ns[0].ID != "test-bucket" || ns[0].Kind != "bucket" {
		t.Fatalf("ns: %+v", ns)
	}
}

func TestS3_ListDocuments_Pagination(t *testing.T) {
	t.Parallel()

	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		call++
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>TOK</NextContinuationToken>`+
				`<Contents><Key>a/1</Key><ETag>"e1"</ETag><LastModified>2024-01-01T00:00:00Z</LastModified><Size>1</Size></Contents>`+
				`<Contents><Key>a/2</Key><ETag>"e2"</ETag><LastModified>2024-01-02T00:00:00Z</LastModified><Size>2</Size></Contents>`+
				`</ListBucketResult>`)

			return
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>a/3</Key><ETag>"e3"</ETag><LastModified>2024-01-03T00:00:00Z</LastModified><Size>3</Size></Contents>`+
			`</ListBucketResult>`)
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, err := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	var keys []string
	for it.Next(context.Background()) {
		keys = append(keys, it.Doc().ID)
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
	if len(keys) != 3 || call != 2 {
		t.Fatalf("keys: %v call: %d", keys, call)
	}
}

func TestS3_ListDocuments_EmptyAndSingle(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, connector.ListOpts{})
	defer func() { _ = it.Close() }()
	if it.Next(context.Background()) {
		t.Fatal("expected no docs")
	}
	if !errors.Is(it.Err(), connector.ErrEndOfPage) {
		t.Fatalf("Err: %v", it.Err())
	}
}

func TestS3_ListDocuments_RateLimited(t *testing.T) {
	t.Parallel()

	// Real AWS S3 surfaces throttling as "503 Slow Down" while
	// MinIO / R2 / Wasabi proxies often use "429 Too Many
	// Requests". Both must wrap connector.ErrRateLimited so the
	// adaptive limiter in internal/connector/adaptive_rate.go
	// can react.
	cases := []struct {
		name   string
		status int
	}{
		{"429_too_many_requests", http.StatusTooManyRequests},
		{"503_slow_down", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)

					return
				}
				http.Error(w, "slow down", tc.status)
			}))
			defer srv.Close()
			c := s3.New(s3.WithHTTPClient(srv.Client()))
			conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
			it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, connector.ListOpts{})
			defer func() { _ = it.Close() }()
			for it.Next(context.Background()) {
			}
			if !errors.Is(it.Err(), connector.ErrRateLimited) {
				t.Fatalf("expected ErrRateLimited for status=%d, got %v", tc.status, it.Err())
			}
		})
	}
}

func TestS3_DeltaSync_RateLimited(t *testing.T) {
	t.Parallel()

	// DeltaSync shares the throttling contract with ListDocuments:
	// both 429 and 503 must wrap ErrRateLimited.
	cases := []struct {
		name   string
		status int
	}{
		{"429_too_many_requests", http.StatusTooManyRequests},
		{"503_slow_down", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)

					return
				}
				http.Error(w, "slow down", tc.status)
			}))
			defer srv.Close()
			c := s3.New(s3.WithHTTPClient(srv.Client()))
			conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
			_, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, "cursor-1")
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("expected ErrRateLimited for status=%d, got %v", tc.status, err)
			}
		})
	}
}

func TestS3_FetchDocument(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		_, _ = fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "test-bucket", ID: "a/1"})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	b, _ := io.ReadAll(doc.Content)
	if string(b) != "hello" {
		t.Fatalf("body: %q", b)
	}
	if doc.Metadata["bucket"] != "test-bucket" {
		t.Fatalf("meta: %+v", doc.Metadata)
	}
}

// TestS3_FetchDocument_EncodesKey locks in the URL-encoding
// contract for object keys carrying URL-significant bytes. S3
// keys may legally contain '?', '#', spaces, '+', and non-ASCII
// — without per-segment percent-encoding (a) Go's URL parser
// eats '?' as query-string and '#' as fragment, (b) spaces fail
// the parse outright, and (c) the SigV4 canonical-URI computed
// from req.URL.EscapedPath() drifts from the path actually sent
// on the wire, producing SignatureDoesNotMatch (403).
//
// The check exercises every documented hazard byte:
//
//   - '/' between segments must pass through un-encoded (logical
//     path separator, not a segment byte).
//   - '?', '#', ' ', '+', and 'é' (non-ASCII rune that encodes to
//     two UTF-8 bytes) must each appear percent-encoded.
//   - r.URL.RawQuery must be empty — proving '?' was treated as
//     a literal key byte, not the query-string separator.
//   - Authorization header must carry an AWS4-HMAC-SHA256
//     signature, proving signing ran over the same encoded path
//     the server received.
func TestS3_FetchDocument_EncodesKey(t *testing.T) {
	t.Parallel()

	const rawKey = "subdir/file ?v=2+x#frag-é.txt"

	captured := make(chan *http.Request, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Clone(r.Context())
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		_, _ = fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Drain the HEAD probe from Connect.
	<-captured

	doc, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{NamespaceID: "test-bucket", ID: rawKey})
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	defer func() { _ = doc.Content.Close() }()
	b, _ := io.ReadAll(doc.Content)
	if string(b) != "hello" {
		t.Fatalf("body: %q", b)
	}
	// The decoded `key` metadata must round-trip the raw key
	// (proving FetchDocument did not mutate the caller-facing ID).
	if got := doc.Metadata["key"]; got != rawKey {
		t.Fatalf("metadata key drift: got %q want %q", got, rawKey)
	}

	req := <-captured
	// r.URL.Path is decoded server-side; if encoding worked,
	// every key byte (including '?' and '#') round-trips here
	// as a literal path byte.
	wantDecoded := "/test-bucket/" + rawKey
	if req.URL.Path != wantDecoded {
		t.Fatalf("decoded path mismatch:\n  got: %q\n want: %q", req.URL.Path, wantDecoded)
	}
	// Critical: '?' inside the key must NOT have been parsed as
	// the query-string separator.
	if req.URL.RawQuery != "" {
		t.Fatalf("expected empty RawQuery, got %q", req.URL.RawQuery)
	}
	// SigV4 canonical-URI = req.URL.EscapedPath(); every hazard
	// byte must appear percent-encoded here so the signature
	// computed in signRequestV4 matches what S3 will hash.
	enc := req.URL.EscapedPath()
	for _, want := range []string{"%3F", "%23", "%20", "%2B", "%C3%A9"} {
		if !strings.Contains(enc, want) {
			t.Fatalf("encoded path missing %q: %q", want, enc)
		}
	}
	// The '/' between "subdir" and "file" must survive un-encoded.
	if !strings.Contains(enc, "subdir/file") {
		t.Fatalf("encoded path lost '/' separator: %q", enc)
	}
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "AWS4-HMAC-SHA256") {
		t.Fatalf("missing SigV4 signature: %q", got)
	}
}

func TestS3_DeltaSync(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		if r.URL.Query().Get("start-after") == "" {
			_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
				`<Contents><Key>a/1</Key><ETag>"e1"</ETag><LastModified>2024-01-01T00:00:00Z</LastModified><Size>1</Size></Contents>`+
				`<Contents><Key>a/2</Key><ETag>"e2"</ETag><LastModified>2024-01-02T00:00:00Z</LastModified><Size>2</Size></Contents>`+
				`</ListBucketResult>`)

			return
		}
		_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>a/3</Key><ETag>"e3"</ETag><LastModified>2024-01-03T00:00:00Z</LastModified><Size>3</Size></Contents>`+
			`</ListBucketResult>`)
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, "")
	if err != nil {
		t.Fatalf("DeltaSync initial: %v", err)
	}
	if cur != "a/2" || len(changes) != 0 {
		t.Fatalf("initial: %v %q", changes, cur)
	}
	changes, cur, err = c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, cur)
	if err != nil {
		t.Fatalf("DeltaSync follow: %v", err)
	}
	if cur != "a/3" || len(changes) != 1 || changes[0].Ref.ID != "a/3" {
		t.Fatalf("follow: %v %q", changes, cur)
	}
}

// TestS3_DeltaSync_BootstrapPaginatesForTrueLastKey locks in
// the fix for the stale-bootstrap bug: list-objects-v2 returns
// at most max-keys (1000) per page, so a single call's last
// key is only the 1000th key lexicographically. For any
// bucket / prefix scope holding more than 1000 objects, using
// that key as the bootstrap cursor would let the next
// DeltaSync call's start-after filter treat every object past
// position 1000 as a "new change" — violating the
// DeltaSyncer contract that an empty cursor returns the
// *current* cursor without backfilling. The mock serves three
// paginated pages (1000 + 1000 + 500 keys) and asserts the
// bootstrap cursor equals the *true* last key (`k/2500`), not
// the first page's last key (`k/1000`).
func TestS3_DeltaSync_BootstrapPaginatesForTrueLastKey(t *testing.T) {
	t.Parallel()

	pages := []struct {
		truncated bool
		next      string
		firstNum  int
		count     int
	}{
		{true, "TOK1", 1, 1000},
		{true, "TOK2", 1001, 1000},
		{false, "", 2001, 500},
	}
	var seenTokens []string
	callIdx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		token := r.URL.Query().Get("continuation-token")
		seenTokens = append(seenTokens, token)
		// Start-after must NEVER be present on the bootstrap
		// pagination walk — that's the steady-state filter
		// and mixing it in would shadow the continuation
		// token. Lock that contract here.
		if r.URL.Query().Get("start-after") != "" {
			t.Errorf("bootstrap pagination must not send start-after; got %q",
				r.URL.Query().Get("start-after"))
		}
		if callIdx >= len(pages) {
			t.Errorf("unexpected extra list-objects-v2 call (token=%q)", token)
			_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)

			return
		}
		p := pages[callIdx]
		callIdx++
		var b strings.Builder
		fmt.Fprintf(&b, `<ListBucketResult><IsTruncated>%v</IsTruncated>`, p.truncated)
		if p.next != "" {
			fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, p.next)
		}
		for i := 0; i < p.count; i++ {
			fmt.Fprintf(&b, `<Contents><Key>k/%04d</Key><ETag>"e"</ETag>`+
				`<LastModified>2024-01-01T00:00:00Z</LastModified><Size>1</Size></Contents>`,
				p.firstNum+i)
		}
		b.WriteString(`</ListBucketResult>`)
		_, _ = w.Write([]byte(b.String()))
	}))
	defer srv.Close()

	c := s3.New(s3.WithHTTPClient(srv.Client()))
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{
		TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	changes, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap must return zero changes; got %d", len(changes))
	}
	if cur != "k/2500" {
		t.Fatalf("bootstrap cursor must equal the true last key k/2500; got %q", cur)
	}
	if callIdx != 3 {
		t.Fatalf("expected 3 paginated list-objects-v2 calls, got %d", callIdx)
	}
	wantTokens := []string{"", "TOK1", "TOK2"}
	if len(seenTokens) != len(wantTokens) {
		t.Fatalf("continuation-token sequence length: got %v, want %v", seenTokens, wantTokens)
	}
	for i, want := range wantTokens {
		if seenTokens[i] != want {
			t.Fatalf("continuation-token[%d]: got %q, want %q", i, seenTokens[i], want)
		}
	}
}

func TestS3_Subscribe_NotSupported(t *testing.T) {
	t.Parallel()

	c := s3.New()
	if _, err := c.Subscribe(context.Background(), nil, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported, got %v", err)
	}
}

func TestS3_SigV4_DeterministicSignature(t *testing.T) {
	t.Parallel()

	// Pin the signing clock so the same request always produces
	// the same Authorization header. This catches any accidental
	// nondeterminism in canonicalisation that would break the
	// real S3 endpoint.
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	captured := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Get("Authorization")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)

			return
		}
		_, _ = fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	defer srv.Close()
	c := s3.New(s3.WithHTTPClient(srv.Client()), s3.WithNow(func() time.Time { return now }))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t, srv.URL)})
	first := <-captured
	_, _ = c.ListNamespaces(context.Background(), conn)
	if _, _, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "test-bucket"}, ""); err != nil {
		t.Fatalf("delta: %v", err)
	}
	second := <-captured
	if first == "" || second == "" {
		t.Fatalf("missing signature: %q %q", first, second)
	}
	if !strings.Contains(first, "AWS4-HMAC-SHA256") || !strings.Contains(second, "AWS4-HMAC-SHA256") {
		t.Fatalf("missing v4 algo: %q %q", first, second)
	}
}

func TestS3_Registered(t *testing.T) {
	t.Parallel()

	if _, err := connector.GetSourceConnector(s3.Name); err != nil {
		t.Fatalf("expected registered, got %v", err)
	}
}
