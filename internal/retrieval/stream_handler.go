package retrieval

// stream_handler.go — Round-6 Task 18.
//
// `POST /v1/retrieve/stream` runs the same retrieval pipeline as
// POST /v1/retrieve but emits a Server-Sent-Events stream so
// clients can render partial results from faster backends before
// the slower ones complete.
//
// Event format:
//
//	event: backend
//	data: {"backend":"<name>","matches":[...]}
//
//	event: backend
//	data: {"backend":"<name>","matches":[...]}
//
//	event: done
//	data: {"matches":[...filtered...]}
//
// Each `backend` event carries the raw matches returned by that
// backend (no merge/policy filter yet). The final `done` event
// carries the post-merge, post-policy, post-privacy-strip matches
// — exactly what /v1/retrieve would return.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// StreamBackend is the narrow surface a backend has to satisfy to
// participate in /v1/retrieve/stream. The production wiring wraps
// the existing fan-out backends.
type StreamBackend interface {
	Name() string
	Search(ctx context.Context, req RetrieveRequest) ([]*Match, error)
}

// StreamMerger is the policy-aware merge step. The handler hands
// it the matches from every backend and gets back the
// fully-filtered, privacy-stripped final list — exactly what the
// /v1/retrieve endpoint would have produced.
type StreamMerger interface {
	Merge(ctx context.Context, tenantID string, req RetrieveRequest, perBackend map[string][]*Match) ([]*Match, error)
}

// StreamHandlerConfig wires the streaming handler.
type StreamHandlerConfig struct {
	Backends []StreamBackend
	Merger   StreamMerger
}

// StreamHandler exposes the SSE endpoint.
type StreamHandler struct {
	cfg StreamHandlerConfig
}

// NewStreamHandler validates and constructs the handler.
func NewStreamHandler(cfg StreamHandlerConfig) (*StreamHandler, error) {
	if len(cfg.Backends) == 0 {
		return nil, errors.New("stream: no backends configured")
	}
	if cfg.Merger == nil {
		return nil, errors.New("stream: nil merger")
	}
	return &StreamHandler{cfg: cfg}, nil
}

// Register mounts the route on rg.
func (h *StreamHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/retrieve/stream", h.stream)
}

// StreamBackendEvent is the body of an `event: backend` SSE frame.
//
// Round-11 Task 7: when the caller sets `Explain: true` on the
// /v1/retrieve/stream request, each backend event carries the
// per-backend timing + a score breakdown for every match emitted
// by that backend. The fields are omitted on non-explain requests
// to keep the wire format minimal for the common case.
type StreamBackendEvent struct {
	Backend  string        `json:"backend"`
	Matches  []*Match      `json:"matches"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration_ns,omitempty"`

	// Explain is populated only when the originating
	// RetrieveRequest had Explain=true. The shape mirrors the
	// per-backend block on RetrieveResponse.Explain so clients
	// can reuse the same renderer for both transports.
	Explain *StreamExplainEvent `json:"explain,omitempty"`
}

// StreamExplainEvent is the per-event explain payload (Round-11
// Task 7). DurationMS is duplicated from StreamBackendEvent.Duration
// for the explain renderer's convenience; ScoreBreakdown lists the
// raw score for each match keyed by chunk ID so the caller can
// surface which backend contributed which signal.
type StreamExplainEvent struct {
	Backend        string             `json:"backend"`
	DurationMS     int64              `json:"duration_ms"`
	HitCount       int                `json:"hit_count"`
	ScoreBreakdown []StreamExplainHit `json:"score_breakdown,omitempty"`
	Error          string             `json:"error,omitempty"`
}

// StreamExplainHit is a single (chunk_id, score) entry on the
// per-backend score breakdown.
type StreamExplainHit struct {
	ChunkID string  `json:"chunk_id"`
	Score   float32 `json:"score"`
}

// StreamDoneEvent is the body of the terminal `event: done` frame.
// Round-11 Task 7: when explain is true, the final event also
// carries an aggregated per-backend timing map so the client can
// reconstruct the same explain shape /v1/retrieve emits.
type StreamDoneEvent struct {
	Matches []*Match `json:"matches"`

	// Explain is populated only when the originating
	// RetrieveRequest had Explain=true.
	Explain *StreamDoneExplain `json:"explain,omitempty"`
}

// StreamDoneExplain is the aggregate explain payload emitted in
// the terminal frame (Round-11 Task 7). The per-backend timing
// duplicates what each backend frame carried, but a client that
// joins mid-stream can still get the full timing roll-up here.
type StreamDoneExplain struct {
	BackendDurationsMS map[string]int64 `json:"backend_durations_ms"`
	BackendHitCounts   map[string]int   `json:"backend_hit_counts"`
}

func (h *StreamHandler) stream(c *gin.Context) {
	tenantID := ""
	if v, ok := c.Get(audit.TenantContextKey); ok {
		tenantID, _ = v.(string)
	}
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req RetrieveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}

	ch := h.runBackends(c.Request.Context(), req)

	per := map[string][]*Match{}
	durations := map[string]int64{}
	hitCounts := map[string]int{}
	for evt := range ch {
		per[evt.Backend] = evt.Matches
		durations[evt.Backend] = evt.Duration.Milliseconds()
		hitCounts[evt.Backend] = len(evt.Matches)
		// Round-11 Task 7: thread per-event explain into the
		// outbound SSE frame so explain-enabled clients can
		// render score/timing without a separate request.
		if req.Explain {
			evt.Explain = buildStreamExplainEvent(evt)
		}
		writeSSE(c.Writer, "backend", evt)
	}

	merged, err := h.cfg.Merger.Merge(c.Request.Context(), tenantID, req, per)
	if err != nil {
		writeSSE(c.Writer, "error", map[string]string{"error": err.Error()})
		return
	}
	done := StreamDoneEvent{Matches: merged}
	if req.Explain {
		done.Explain = &StreamDoneExplain{
			BackendDurationsMS: durations,
			BackendHitCounts:   hitCounts,
		}
	}
	writeSSE(c.Writer, "done", done)
}

// buildStreamExplainEvent projects a backend frame into the
// explain shape clients render. Empty error payloads are kept so
// the renderer can show "vector: 12ms (failed: ...)" symmetrically
// to non-error backends.
func buildStreamExplainEvent(evt StreamBackendEvent) *StreamExplainEvent {
	out := &StreamExplainEvent{
		Backend:    evt.Backend,
		DurationMS: evt.Duration.Milliseconds(),
		HitCount:   len(evt.Matches),
		Error:      evt.Error,
	}
	if len(evt.Matches) > 0 {
		out.ScoreBreakdown = make([]StreamExplainHit, 0, len(evt.Matches))
		for _, m := range evt.Matches {
			if m == nil {
				continue
			}
			out.ScoreBreakdown = append(out.ScoreBreakdown, StreamExplainHit{
				ChunkID: m.ID,
				Score:   m.Score,
			})
		}
	}
	return out
}

// runBackends fans the request out to every backend in parallel
// and returns a channel that closes once every backend has
// reported in.
func (h *StreamHandler) runBackends(ctx context.Context, req RetrieveRequest) <-chan StreamBackendEvent {
	out := make(chan StreamBackendEvent, len(h.cfg.Backends))
	var wg sync.WaitGroup
	for _, b := range h.cfg.Backends {
		wg.Add(1)
		go func(b StreamBackend) {
			defer wg.Done()
			start := time.Now()
			matches, err := b.Search(ctx, req)
			evt := StreamBackendEvent{
				Backend:  b.Name(),
				Matches:  matches,
				Duration: time.Since(start),
			}
			if err != nil {
				evt.Error = err.Error()
			}
			out <- evt
		}(b)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// writeSSE emits an SSE frame and flushes immediately.
func writeSSE(w http.ResponseWriter, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{"error":"marshal failed"}`)
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
