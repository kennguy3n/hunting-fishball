package retrieval_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

type stubStreamBackend struct {
	name    string
	matches []*retrieval.Match
	delay   time.Duration
}

func (s *stubStreamBackend) Name() string { return s.name }
func (s *stubStreamBackend) Search(ctx context.Context, _ retrieval.RetrieveRequest) ([]*retrieval.Match, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.matches, nil
}

type stubMerger struct{}

func (stubMerger) Merge(_ context.Context, _ string, _ retrieval.RetrieveRequest, per map[string][]*retrieval.Match) ([]*retrieval.Match, error) {
	var out []*retrieval.Match
	for _, m := range per {
		out = append(out, m...)
	}
	return out, nil
}

func stubStreamRouter(tenantID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
	})
	return r
}

func TestStreamHandler_Validation(t *testing.T) {
	if _, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{Merger: stubMerger{}}); err == nil {
		t.Fatalf("expected error for empty backends")
	}
	if _, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{&stubStreamBackend{name: "x"}},
	}); err == nil {
		t.Fatalf("expected error for nil merger")
	}
}

func TestStreamHandler_SSEFormat(t *testing.T) {
	be1 := &stubStreamBackend{name: "vector", matches: []*retrieval.Match{{ID: "c1"}}}
	be2 := &stubStreamBackend{name: "bm25", matches: []*retrieval.Match{{ID: "c2"}}}
	h, err := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{be1, be2}, Merger: stubMerger{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := stubStreamRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body := strings.NewReader(`{"query":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("wrong content-type: %s", ct)
	}
	// Expect 2 backend events + 1 done event.
	var (
		backends int
		sawDone  bool
	)
	scanner := bufio.NewScanner(strings.NewReader(rr.Body.String()))
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	var current string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			current = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			switch current {
			case "backend":
				var ev retrieval.StreamBackendEvent
				_ = json.Unmarshal([]byte(data), &ev)
				if ev.Backend == "" {
					t.Fatalf("backend event missing name: %s", data)
				}
				backends++
			case "done":
				var ev retrieval.StreamDoneEvent
				_ = json.Unmarshal([]byte(data), &ev)
				if len(ev.Matches) != 2 {
					t.Fatalf("done event match count=%d want 2; data=%s", len(ev.Matches), data)
				}
				sawDone = true
			}
		}
	}
	if backends != 2 {
		t.Fatalf("expected 2 backend events; got %d", backends)
	}
	if !sawDone {
		t.Fatalf("missing done event")
	}
}

func TestStreamHandler_MissingTenant(t *testing.T) {
	h, _ := retrieval.NewStreamHandler(retrieval.StreamHandlerConfig{
		Backends: []retrieval.StreamBackend{&stubStreamBackend{name: "vector"}},
		Merger:   stubMerger{},
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve/stream", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401; got %d", rr.Code)
	}
}
