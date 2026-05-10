// Package admin — sync_progress_stream.go ships an SSE endpoint
// that streams sync progress for a (tenant, source).
//
// Round-4 Task 18: the existing GET /v1/admin/sources/:id/sync
// returns a single snapshot. SREs and the desktop UI need a live
// feed so the user can watch a long-running sync without
// hammering the polling endpoint. This handler does the polling
// once per (StreamPollInterval) and pushes deltas as SSE events.
//
// Event names match the metric names so a client can switch on
// `event:` in EventSource.addEventListener:
//
//	discovered  { namespace_id, total }
//	processed   { namespace_id, total }
//	failed      { namespace_id, total }
//	completed   { source_id }              fired when discovered ==
//	                                       processed + failed and
//	                                       all changed counters
//	                                       have settled.
//	heartbeat   {}                         every HeartbeatInterval
//	                                       so reverse proxies don't
//	                                       drop the connection.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// StreamPollInterval is how often the SSE goroutine polls the
// store. Five seconds keeps DB load minimal while remaining
// responsive enough for an interactive admin UI.
const StreamPollInterval = 5 * time.Second

// HeartbeatInterval is the maximum gap between any two events on
// the wire. Below most idle-proxy thresholds (NGINX 60s default).
const HeartbeatInterval = 30 * time.Second

// CompletionGracePeriod is how long the stream waits after the
// totals appear settled before sending `completed` and closing.
// Allows in-flight deltas to land on the store.
const CompletionGracePeriod = 2 * time.Second

// ProgressReader is the narrow contract the SSE handler depends
// on. SyncProgressStoreGORM satisfies it via List.
type ProgressReader interface {
	List(ctx context.Context, tenantID, sourceID string) ([]SyncProgress, error)
}

// ProgressStreamHandler serves the SSE endpoint.
type ProgressStreamHandler struct {
	reader ProgressReader

	// Poll overrides StreamPollInterval (test seam).
	Poll time.Duration

	// Heartbeat overrides HeartbeatInterval (test seam).
	Heartbeat time.Duration
}

// NewProgressStreamHandler constructs the handler.
func NewProgressStreamHandler(reader ProgressReader) *ProgressStreamHandler {
	return &ProgressStreamHandler{reader: reader}
}

// Register mounts the endpoint.
func (h *ProgressStreamHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/sources/:id/sync/stream", h.stream)
}

func (h *ProgressStreamHandler) stream(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	if sourceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}

	// SSE headers.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // tell nginx not to buffer

	pollInterval := h.Poll
	if pollInterval <= 0 {
		pollInterval = StreamPollInterval
	}
	heartbeatInterval := h.Heartbeat
	if heartbeatInterval <= 0 {
		heartbeatInterval = HeartbeatInterval
	}

	last := map[string]progressKey{}
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	settledAt := time.Time{}
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-heartbeat.C:
			h.send(c, "heartbeat", map[string]any{"ts": time.Now().UTC()})
		case <-pollTicker.C:
			rows, err := h.reader.List(c.Request.Context(), tenantID, sourceID)
			if err != nil {
				h.send(c, "error", map[string]any{"error": err.Error()})
				return
			}
			settled := true
			for _, row := range rows {
				key := row.NamespaceID
				cur := progressKey{
					Discovered: row.Discovered,
					Processed:  row.Processed,
					Failed:     row.Failed,
				}
				prev := last[key]
				if cur.Discovered != prev.Discovered {
					h.send(c, "discovered", map[string]any{
						"namespace_id": row.NamespaceID,
						"total":        row.Discovered,
					})
				}
				if cur.Processed != prev.Processed {
					h.send(c, "processed", map[string]any{
						"namespace_id": row.NamespaceID,
						"total":        row.Processed,
					})
				}
				if cur.Failed != prev.Failed {
					h.send(c, "failed", map[string]any{
						"namespace_id": row.NamespaceID,
						"total":        row.Failed,
					})
				}
				last[key] = cur
				if cur.Discovered == 0 || cur.Processed+cur.Failed < cur.Discovered {
					settled = false
				}
			}
			if settled && len(rows) > 0 {
				if settledAt.IsZero() {
					settledAt = time.Now()
				} else if time.Since(settledAt) >= CompletionGracePeriod {
					h.send(c, "completed", map[string]any{"source_id": sourceID})
					return
				}
			} else {
				settledAt = time.Time{}
			}
		}
	}
}

// progressKey is the per-namespace counter snapshot. Used to
// detect deltas without re-reading the row each time.
type progressKey struct {
	Discovered int64
	Processed  int64
	Failed     int64
}

// send emits one SSE event. The wire format is the documented
// "event: <name>\n data: <json>\n\n" pattern; we flush on every
// event so a client receives them with sub-poll-interval latency.
func (h *ProgressStreamHandler) send(c *gin.Context, eventName string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		// Encoding errors here are bugs; fail loudly via the wire so
		// the client can reconnect.
		_, _ = fmt.Fprintf(c.Writer, "event: error\n")
		_, _ = fmt.Fprintf(c.Writer, "data: %q\n\n", err.Error())
		return
	}
	// Sanitize event name (must not contain CR/LF per the SSE spec).
	eventName = strings.ReplaceAll(eventName, "\n", "")
	eventName = strings.ReplaceAll(eventName, "\r", "")
	_, _ = fmt.Fprintf(c.Writer, "event: %s\n", eventName)
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", body)
	c.Writer.Flush()
}
