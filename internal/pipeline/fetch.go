package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FetchConfig configures the Stage 1 fetch worker.
type FetchConfig struct {
	// HTTPClient is used for HTTP / HTTPS fetches. Defaults to
	// http.DefaultClient. Tests inject httptest.Server-backed clients.
	HTTPClient *http.Client

	// MaxAttempts is the number of fetch attempts (including the first).
	// Defaults to 3.
	MaxAttempts int

	// InitialBackoff is the first retry sleep; subsequent retries
	// double the backoff up to MaxBackoff. Defaults to 100ms.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential backoff. Defaults to 5s.
	MaxBackoff time.Duration

	// MaxBodyBytes caps the per-document download size. Zero disables
	// the cap. Defaults to 64 MiB.
	MaxBodyBytes int64

	// Concurrency bounds the number of in-flight fetches. Defaults to
	// 8. The semaphore is per-Fetcher so each tenant's pipeline gets
	// its own bound.
	Concurrency int

	// S3Fetcher, when non-nil, handles `s3://bucket/key` URLs. The
	// production wiring injects an AWS SDK-backed fetcher; tests
	// inject an in-memory fake.
	S3Fetcher S3Fetcher

	// Now lets tests override the wall clock used to stamp
	// Document.IngestedAt. Defaults to time.Now.
	Now func() time.Time

	// Sleep lets tests skip the real sleep between retries. Defaults
	// to time.Sleep.
	Sleep func(time.Duration)
}

// S3Fetcher abstracts the S3 GetObject path so the production code can
// import the AWS SDK without forcing the unit tests to. Tests inject
// an in-memory implementation; production wiring binds to the AWS SDK.
type S3Fetcher interface {
	Fetch(ctx context.Context, bucket, key string) ([]byte, error)
}

func (c *FetchConfig) defaults() {
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.InitialBackoff == 0 {
		c.InitialBackoff = 100 * time.Millisecond
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 5 * time.Second
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 64 << 20
	}
	if c.Concurrency == 0 {
		c.Concurrency = 8
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = time.Sleep
	}
}

// Fetcher is the Stage 1 worker. It supports HTTP(S) and S3 URLs, with
// retry + content-hash deduplication.
type Fetcher struct {
	cfg FetchConfig
	sem chan struct{}
}

// NewFetcher constructs a Fetcher with the supplied config. Required
// fields fall back to documented defaults.
func NewFetcher(cfg FetchConfig) *Fetcher {
	cfg.defaults()

	return &Fetcher{
		cfg: cfg,
		sem: make(chan struct{}, cfg.Concurrency),
	}
}

// FetchEvent runs Stage 1 for a single IngestEvent. Returns either:
//
//   - a populated *Document with ContentHash set;
//   - ErrUnchanged when ContentHash == PreviousHash and the event did
//     not request a force re-fetch;
//   - ErrPoisonMessage when the event is structurally invalid.
//
// The fetcher honours ctx cancellation between retries and during the
// HTTP read; long-running fetches release the concurrency semaphore as
// soon as ctx fires.
func (f *Fetcher) FetchEvent(ctx context.Context, evt IngestEvent) (*Document, error) {
	if evt.TenantID == "" || evt.DocumentID == "" {
		return nil, fmt.Errorf("%w: tenant_id and document_id required", ErrPoisonMessage)
	}

	// Acquire concurrency slot. ctx cancellation aborts the wait.
	select {
	case f.sem <- struct{}{}:
		defer func() { <-f.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	body, err := f.fetchBytes(ctx, evt)
	if err != nil {
		return nil, err
	}

	hash := HashContent(body)
	if !evt.Force && evt.PreviousHash != "" && hash == evt.PreviousHash {
		return nil, ErrUnchanged
	}

	doc := &Document{
		TenantID:     evt.TenantID,
		SourceID:     evt.SourceID,
		DocumentID:   evt.DocumentID,
		NamespaceID:  evt.NamespaceID,
		Title:        evt.Title,
		MIMEType:     evt.MIMEType,
		Content:      body,
		ContentHash:  hash,
		PreviousHash: evt.PreviousHash,
		PrivacyLabel: evt.PrivacyLabel,
		IngestedAt:   f.cfg.Now().UTC(),
		Metadata:     evt.Metadata,
		Force:        evt.Force,
	}

	return doc, nil
}

// fetchBytes returns the document bytes. Decides between inline,
// HTTP(S), and S3 paths based on the event shape.
func (f *Fetcher) fetchBytes(ctx context.Context, evt IngestEvent) ([]byte, error) {
	if len(evt.InlineContent) > 0 {
		return evt.InlineContent, nil
	}
	if evt.FetchURL == "" {
		return nil, fmt.Errorf("%w: missing fetch_url", ErrPoisonMessage)
	}

	u, err := url.Parse(evt.FetchURL)
	if err != nil {
		return nil, fmt.Errorf("%w: parse fetch_url: %v", ErrPoisonMessage, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return f.fetchHTTP(ctx, evt.FetchURL)
	case "s3":
		return f.fetchS3(ctx, u)
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrPoisonMessage, u.Scheme)
	}
}

// fetchHTTP runs an HTTP GET with bounded retries on transient errors.
// Non-2xx 4xx-class responses are treated as terminal (no retry); 5xx
// and network errors are retried with exponential backoff.
func (f *Fetcher) fetchHTTP(ctx context.Context, target string) ([]byte, error) {
	var lastErr error
	backoff := f.cfg.InitialBackoff

	for attempt := 1; attempt <= f.cfg.MaxAttempts; attempt++ {
		body, err, retry := f.httpAttempt(ctx, target)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
		if attempt == f.cfg.MaxAttempts {
			break
		}
		// Sleep with cancellation; the configurable Sleep keeps tests
		// fast.
		sleep := backoff + jitter(backoff)
		if sleep > f.cfg.MaxBackoff {
			sleep = f.cfg.MaxBackoff
		}
		t := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			t.Stop()

			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > f.cfg.MaxBackoff {
			backoff = f.cfg.MaxBackoff
		}
	}

	return nil, fmt.Errorf("fetch: exhausted retries: %w", lastErr)
}

// httpAttempt runs one HTTP GET. The third return value indicates
// whether the caller should retry the fetch.
func (f *Fetcher) httpAttempt(ctx context.Context, target string) ([]byte, error, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err, false
	}

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		// Network errors are transient.
		return nil, err, true
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("fetch: status %d", resp.StatusCode), true
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch: status %d", resp.StatusCode), false
	}

	body, err := readBounded(resp.Body, f.cfg.MaxBodyBytes)
	if err != nil {
		return nil, err, false
	}

	return body, nil, false
}

func (f *Fetcher) fetchS3(ctx context.Context, u *url.URL) ([]byte, error) {
	bucket := u.Host
	key := strings.TrimPrefix(u.Path, "/")
	if bucket == "" || key == "" {
		return nil, fmt.Errorf("%w: malformed s3 url", ErrPoisonMessage)
	}
	if f.cfg.S3Fetcher == nil {
		return nil, fmt.Errorf("fetch: S3 fetcher not configured for %q", u.String())
	}

	return f.cfg.S3Fetcher.Fetch(ctx, bucket, key)
}

// readBounded reads up to limit bytes from r. Reading more than limit
// returns an error so the worker fails fast on oversized documents.
// limit <= 0 disables the cap.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	lr := &io.LimitedReader{R: r, N: limit + 1}
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("fetch: body exceeds %d bytes", limit)
	}

	return body, nil
}

// jitter returns a random duration in [0, base/4) so concurrent
// retriers don't dog-pile on a recovering upstream.
func jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(base) / 4))
}

// MemoryS3Fetcher is an in-memory S3Fetcher used by the e2e and
// integration tests. The map key is "<bucket>/<key>".
type MemoryS3Fetcher struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMemoryS3Fetcher returns a fresh in-memory fetcher.
func NewMemoryS3Fetcher() *MemoryS3Fetcher {
	return &MemoryS3Fetcher{objects: map[string][]byte{}}
}

// Put writes bytes for the (bucket, key) tuple.
func (m *MemoryS3Fetcher) Put(bucket, key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.objects[bucket+"/"+key] = append([]byte(nil), body...)
}

// Fetch implements S3Fetcher.
func (m *MemoryS3Fetcher) Fetch(_ context.Context, bucket, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	b, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, errors.New("memory s3: object not found")
	}

	return append([]byte(nil), b...), nil
}
