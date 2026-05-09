package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestFetcher(t *testing.T, cfg FetchConfig) *Fetcher {
	t.Helper()
	cfg.Sleep = func(time.Duration) {}
	cfg.InitialBackoff = time.Microsecond
	cfg.MaxBackoff = time.Microsecond
	cfg.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }

	return NewFetcher(cfg)
}

func TestFetcher_FetchHTTP_Happy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client()})
	doc, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID:   "tenant-a",
		SourceID:   "src-1",
		DocumentID: "doc-1",
		FetchURL:   srv.URL,
	})
	if err != nil {
		t.Fatalf("FetchEvent: %v", err)
	}
	if string(doc.Content) != "hello world" {
		t.Fatalf("body: %q", doc.Content)
	}
	if doc.ContentHash == "" {
		t.Fatal("ContentHash empty")
	}
	if doc.IngestedAt.IsZero() {
		t.Fatal("IngestedAt zero")
	}
}

func TestFetcher_FetchHTTP_RetriesOn5xx(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client(), MaxAttempts: 3})
	doc, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d", FetchURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("FetchEvent: %v", err)
	}
	if string(doc.Content) != "ok" {
		t.Fatalf("body: %q", doc.Content)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls: %d, want 3", got)
	}
}

func TestFetcher_FetchHTTP_NoRetryOn4xx(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client(), MaxAttempts: 5})
	_, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d", FetchURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", got)
	}
}

func TestFetcher_FetchHTTP_ContentHashShortCircuit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("same body"))
	}))
	defer srv.Close()

	hash := HashContent([]byte("same body"))

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client()})
	_, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID:     "t",
		SourceID:     "s",
		DocumentID:   "d",
		FetchURL:     srv.URL,
		PreviousHash: hash,
	})
	if !errors.Is(err, ErrUnchanged) {
		t.Fatalf("expected ErrUnchanged, got %v", err)
	}

	// Force=true must fetch even when hash matches.
	doc, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d",
		FetchURL: srv.URL, PreviousHash: hash, Force: true,
	})
	if err != nil {
		t.Fatalf("force fetch: %v", err)
	}
	if doc == nil {
		t.Fatal("force fetch returned nil doc")
	}
}

func TestFetcher_FetchInline(t *testing.T) {
	t.Parallel()

	f := newTestFetcher(t, FetchConfig{})
	doc, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d",
		InlineContent: []byte("inline payload"),
	})
	if err != nil {
		t.Fatalf("FetchEvent: %v", err)
	}
	if string(doc.Content) != "inline payload" {
		t.Fatalf("body: %q", doc.Content)
	}
}

func TestFetcher_FetchS3(t *testing.T) {
	t.Parallel()

	mem := NewMemoryS3Fetcher()
	mem.Put("bkt", "k", []byte("from s3"))

	f := newTestFetcher(t, FetchConfig{S3Fetcher: mem})
	doc, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d",
		FetchURL: "s3://bkt/k",
	})
	if err != nil {
		t.Fatalf("FetchEvent: %v", err)
	}
	if string(doc.Content) != "from s3" {
		t.Fatalf("body: %q", doc.Content)
	}

	if _, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d2",
		FetchURL: "s3://bkt/missing",
	}); err == nil {
		t.Fatal("expected error on missing s3 object")
	}
}

func TestFetcher_PoisonMessage(t *testing.T) {
	t.Parallel()

	f := newTestFetcher(t, FetchConfig{})
	for _, tc := range []struct {
		name string
		evt  IngestEvent
	}{
		{"missing tenant", IngestEvent{DocumentID: "d", FetchURL: "http://x"}},
		{"missing doc id", IngestEvent{TenantID: "t", FetchURL: "http://x"}},
		{"missing url", IngestEvent{TenantID: "t", DocumentID: "d"}},
		{"unsupported scheme", IngestEvent{TenantID: "t", DocumentID: "d", FetchURL: "ftp://x/y"}},
		{"malformed s3", IngestEvent{TenantID: "t", DocumentID: "d", FetchURL: "s3://"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := f.FetchEvent(context.Background(), tc.evt)
			if !errors.Is(err, ErrPoisonMessage) {
				t.Fatalf("expected ErrPoisonMessage, got %v", err)
			}
		})
	}
}

func TestFetcher_BodyTooLarge(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 1024))
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client(), MaxBodyBytes: 100})
	_, err := f.FetchEvent(context.Background(), IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d", FetchURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error on oversized body")
	}
}

func TestFetcher_ContextCancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client()})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := f.FetchEvent(ctx, IngestEvent{
		TenantID: "t", SourceID: "s", DocumentID: "d", FetchURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestFetcher_ConcurrencyLimit(t *testing.T) {
	t.Parallel()

	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for {
			cur := peak.Load()
			if cur >= now || peak.CompareAndSwap(cur, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := newTestFetcher(t, FetchConfig{HTTPClient: srv.Client(), Concurrency: 2})

	const n = 10
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := f.FetchEvent(context.Background(), IngestEvent{
				TenantID: "t", SourceID: "s", DocumentID: fmt.Sprintf("d-%d", i),
				FetchURL: srv.URL,
			})
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("FetchEvent: %v", err)
		}
	}
	if got := peak.Load(); got > 2 {
		t.Fatalf("concurrency exceeded: peak=%d, want <=2", got)
	}
}
