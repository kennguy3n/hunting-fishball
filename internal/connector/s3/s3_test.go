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
