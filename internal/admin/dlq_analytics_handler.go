// dlq_analytics_handler.go — Round-18 Task 12.
//
// GET /v1/admin/dlq/analytics returns the aggregated rollup the
// admin portal needs to render the DLQ dashboard:
//
//   - by_category   — count per DLQCategoryTransient / Permanent /
//                     Unknown so operators can see the
//                     auto-replayable vs poison-message split.
//   - top_errors    — most common error_text strings (truncated to
//                     200 chars) so operators can deduplicate.
//   - by_connector  — failure distribution by source connector so
//                     ops can pin a single noisy connector quickly.
//   - by_hour       — failure rate over the last 24h binned hourly
//                     so the portal can render a sparkline.
//
// The handler reads through the standard tenant scope: tenant_id
// comes from the gin context and is never read from the body or
// path.

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// DLQAnalyticsLister is the storage seam — the in-process unit
// tests pass a fake; production passes *pipeline.DLQStoreGORM via
// the existing DLQReader interface widened to support sweeps with
// a Since filter.
type DLQAnalyticsLister interface {
	List(ctx context.Context, filter pipeline.DLQListFilter) ([]pipeline.DLQMessage, error)
}

// DLQAnalyticsHandlerConfig configures DLQAnalyticsHandler.
type DLQAnalyticsHandlerConfig struct {
	Reader DLQAnalyticsLister

	// Now overrides time.Now for tests. Optional.
	Now func() time.Time

	// MaxScan caps the list scan. Defaults to 2000. The handler
	// rolls up over whatever it scans — operators querying the
	// last 24h on a high-throughput tenant should set a
	// max_scan=N param to widen the window.
	MaxScan int
}

// DLQAnalyticsHandler serves GET /v1/admin/dlq/analytics.
type DLQAnalyticsHandler struct {
	cfg DLQAnalyticsHandlerConfig
}

// NewDLQAnalyticsHandler validates cfg and returns a handler.
func NewDLQAnalyticsHandler(cfg DLQAnalyticsHandlerConfig) (*DLQAnalyticsHandler, error) {
	if cfg.Reader == nil {
		return nil, errors.New("admin dlq analytics: nil Reader")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxScan == 0 {
		cfg.MaxScan = 2000
	}

	return &DLQAnalyticsHandler{cfg: cfg}, nil
}

// Register mounts the route on rg.
func (h *DLQAnalyticsHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/dlq/analytics", h.get)
}

// DLQAnalyticsResponse is the JSON shape returned to callers.
type DLQAnalyticsResponse struct {
	TenantID     string                  `json:"tenant_id"`
	WindowFrom   time.Time               `json:"window_from"`
	WindowTo     time.Time               `json:"window_to"`
	Total        int                     `json:"total"`
	ByCategory   map[string]int          `json:"by_category"`
	ByConnector  map[string]int          `json:"by_connector"`
	TopErrors    []DLQAnalyticsTopError  `json:"top_errors"`
	ByHour       []DLQAnalyticsHourPoint `json:"by_hour"`
	ScanExceeded bool                    `json:"scan_exceeded"`
}

// DLQAnalyticsTopError is one row of the deduped error histogram.
type DLQAnalyticsTopError struct {
	ErrorText string `json:"error_text"`
	Count     int    `json:"count"`
}

// DLQAnalyticsHourPoint is one bucket in the per-hour failure rate.
type DLQAnalyticsHourPoint struct {
	HourStart time.Time `json:"hour_start"`
	Count     int       `json:"count"`
}

func (h *DLQAnalyticsHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	now := h.cfg.Now().UTC()
	from := now.Add(-24 * time.Hour)
	if v := c.Query("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			from = t.UTC()
		}
	}
	maxScan := h.cfg.MaxScan
	if v := c.Query("max_scan"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxScan = n
		}
	}
	rows, err := h.cfg.Reader.List(c.Request.Context(), pipeline.DLQListFilter{
		TenantID:        tenantID,
		IncludeReplayed: false,
		PageSize:        maxScan,
		MinCreatedAt:    from,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}

	resp := DLQAnalyticsResponse{
		TenantID:    tenantID,
		WindowFrom:  from,
		WindowTo:    now,
		ByCategory:  map[string]int{},
		ByConnector: map[string]int{},
	}
	errCounts := map[string]int{}
	hourBuckets := map[time.Time]int{}
	for _, r := range rows {
		if r.FailedAt.Before(from) {
			continue
		}
		resp.Total++
		cat := r.Category
		if cat == "" {
			cat = pipeline.DLQCategoryUnknown
		}
		resp.ByCategory[cat]++
		conn := connectorFromDLQ(r)
		if conn != "" {
			resp.ByConnector[conn]++
		}
		errText := truncate(r.ErrorText, 200)
		errCounts[errText]++
		hourBuckets[r.FailedAt.UTC().Truncate(time.Hour)]++
	}
	if len(rows) == maxScan {
		resp.ScanExceeded = true
	}

	resp.TopErrors = topErrors(errCounts, 10)
	resp.ByHour = hourBuckets3(hourBuckets, from, now)

	c.JSON(http.StatusOK, resp)
}

// connectorFromDLQ tries to extract the connector hint from the
// DLQMessage payload. Returns "" when the payload is opaque.
func connectorFromDLQ(r pipeline.DLQMessage) string {
	if len(r.Payload) == 0 {
		return ""
	}
	var probe struct {
		Connector string `json:"connector"`
		Source    string `json:"source"`
	}
	if err := json.Unmarshal(r.Payload, &probe); err == nil {
		if probe.Connector != "" {
			return probe.Connector
		}
		if probe.Source != "" {
			return probe.Source
		}
	}

	return ""
}

// truncate caps s at n runes (not bytes) so the result is always
// valid UTF-8 even when the cut falls inside a multi-byte
// codepoint. Byte-level slicing would produce a replacement
// character (U+FFFD) once the JSON encoder hits the split.
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}

	return string(rs[:n]) + "…"
}

func topErrors(counts map[string]int, k int) []DLQAnalyticsTopError {
	out := make([]DLQAnalyticsTopError, 0, len(counts))
	for txt, n := range counts {
		out = append(out, DLQAnalyticsTopError{ErrorText: txt, Count: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}

		return out[i].ErrorText < out[j].ErrorText
	})
	if len(out) > k {
		out = out[:k]
	}

	return out
}

func hourBuckets3(buckets map[time.Time]int, from, to time.Time) []DLQAnalyticsHourPoint {
	out := []DLQAnalyticsHourPoint{}
	start := from.UTC().Truncate(time.Hour)
	end := to.UTC().Truncate(time.Hour)
	for t := start; !t.After(end); t = t.Add(time.Hour) {
		out = append(out, DLQAnalyticsHourPoint{HourStart: t, Count: buckets[t]})
	}

	return out
}
