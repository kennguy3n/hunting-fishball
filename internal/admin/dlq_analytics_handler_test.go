package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type fakeAnalyticsStore struct {
	rows []pipeline.DLQMessage
}

func (s *fakeAnalyticsStore) List(_ context.Context, f pipeline.DLQListFilter) ([]pipeline.DLQMessage, error) {
	out := []pipeline.DLQMessage{}
	for _, r := range s.rows {
		if r.TenantID != f.TenantID {
			continue
		}
		if !f.MinCreatedAt.IsZero() && r.FailedAt.Before(f.MinCreatedAt) {
			continue
		}
		out = append(out, r)
	}

	return out, nil
}

func setupAnalyticsRouter(t *testing.T, store admin.DLQAnalyticsLister, tenantID string, now time.Time) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewDLQAnalyticsHandler(admin.DLQAnalyticsHandlerConfig{
		Reader: store,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Next()
	})
	h.Register(rg)

	return r
}

func TestDLQAnalytics_RollsUpByCategoryConnectorAndHour(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeAnalyticsStore{
		rows: []pipeline.DLQMessage{
			{ID: "1", TenantID: "t1", Category: pipeline.DLQCategoryTransient, ErrorText: "connection refused", FailedAt: now.Add(-30 * time.Minute), Payload: []byte(`{"connector":"slack"}`)},
			{ID: "2", TenantID: "t1", Category: pipeline.DLQCategoryTransient, ErrorText: "connection refused", FailedAt: now.Add(-2 * time.Hour), Payload: []byte(`{"connector":"slack"}`)},
			{ID: "3", TenantID: "t1", Category: pipeline.DLQCategoryPermanent, ErrorText: "parse error: bad json", FailedAt: now.Add(-3 * time.Hour), Payload: []byte(`{"connector":"github"}`)},
			{ID: "4", TenantID: "t1", Category: pipeline.DLQCategoryUnknown, ErrorText: "", FailedAt: now.Add(-12 * time.Hour), Payload: []byte(`{"source":"jira"}`)},
			{ID: "5", TenantID: "t1", Category: pipeline.DLQCategoryTransient, ErrorText: "connection refused", FailedAt: now.Add(-48 * time.Hour), Payload: []byte(`{"connector":"slack"}`)}, // outside window
		},
	}
	r := setupAnalyticsRouter(t, store, "t1", now)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/analytics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.DLQAnalyticsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Total != 4 {
		t.Fatalf("expected total 4 (5th is outside window), got %d", resp.Total)
	}
	if resp.ByCategory[pipeline.DLQCategoryTransient] != 2 {
		t.Fatalf("expected transient=2, got %+v", resp.ByCategory)
	}
	if resp.ByCategory[pipeline.DLQCategoryPermanent] != 1 {
		t.Fatalf("expected permanent=1, got %+v", resp.ByCategory)
	}
	if resp.ByConnector["slack"] != 2 || resp.ByConnector["github"] != 1 || resp.ByConnector["jira"] != 1 {
		t.Fatalf("connector counts wrong: %+v", resp.ByConnector)
	}
	if len(resp.TopErrors) == 0 || resp.TopErrors[0].ErrorText != "connection refused" {
		t.Fatalf("expected top error 'connection refused', got %+v", resp.TopErrors)
	}
	if len(resp.ByHour) != 25 {
		t.Fatalf("expected 25 hourly buckets (24h + edge), got %d", len(resp.ByHour))
	}
}

func TestDLQAnalytics_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewDLQAnalyticsHandler(admin.DLQAnalyticsHandlerConfig{
		Reader: &fakeAnalyticsStore{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/analytics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestDLQAnalytics_HonoursSinceQueryParam(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeAnalyticsStore{
		rows: []pipeline.DLQMessage{
			{ID: "1", TenantID: "t1", Category: pipeline.DLQCategoryTransient, ErrorText: "x", FailedAt: now.Add(-5 * time.Minute), Payload: []byte(`{}`)},
			{ID: "2", TenantID: "t1", Category: pipeline.DLQCategoryTransient, ErrorText: "x", FailedAt: now.Add(-2 * time.Hour), Payload: []byte(`{}`)},
		},
	}
	r := setupAnalyticsRouter(t, store, "t1", now)
	w := httptest.NewRecorder()
	since := now.Add(-30 * time.Minute).Format(time.RFC3339)
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/analytics?since="+since, nil))
	var resp admin.DLQAnalyticsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Total != 1 {
		t.Fatalf("expected 1 row in narrowed window, got %d body=%s", resp.Total, w.Body.String())
	}
}

func TestDLQAnalytics_TruncatesLongErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	long := strings.Repeat("e", 500)
	store := &fakeAnalyticsStore{
		rows: []pipeline.DLQMessage{
			{ID: "1", TenantID: "t1", Category: pipeline.DLQCategoryUnknown, ErrorText: long, FailedAt: now.Add(-1 * time.Hour), Payload: []byte(`{}`)},
		},
	}
	r := setupAnalyticsRouter(t, store, "t1", now)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/analytics", nil))
	var resp admin.DLQAnalyticsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.TopErrors) != 1 {
		t.Fatalf("expected 1 top error, got %+v", resp.TopErrors)
	}
	if len(resp.TopErrors[0].ErrorText) > 220 {
		t.Fatalf("expected truncated error text, got %d chars", len(resp.TopErrors[0].ErrorText))
	}
}

// TestDLQAnalytics_TruncatesMultibyteUTF8 pins the regression
// from Devin Review on c0846d0: byte-level truncation can split
// a multi-byte UTF-8 codepoint and JSON-encode the result with
// U+FFFD (REPLACEMENT CHARACTER). The fix is a rune-level cut.
//
// The error string is built so that the 200th *byte* falls
// inside the 3-byte UTF-8 sequence for U+4E2D (中). With a
// byte-level slice the JSON response would contain "\ufffd";
// with a rune-level cut it must be free of replacement chars.
func TestDLQAnalytics_TruncatesMultibyteUTF8(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 198 single-byte runes, then a 3-byte rune ('中'). Byte 198
	// is the start of the 3-byte sequence, so byte 200 lands
	// inside it.
	mixed := strings.Repeat("a", 198) + "中" + strings.Repeat("中", 50)
	store := &fakeAnalyticsStore{
		rows: []pipeline.DLQMessage{
			{ID: "1", TenantID: "t1", Category: pipeline.DLQCategoryUnknown, ErrorText: mixed, FailedAt: now.Add(-1 * time.Hour), Payload: []byte(`{}`)},
		},
	}
	r := setupAnalyticsRouter(t, store, "t1", now)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/dlq/analytics", nil))
	if strings.Contains(w.Body.String(), `\ufffd`) || strings.ContainsRune(w.Body.String(), '\uFFFD') {
		t.Fatalf("response contains U+FFFD; truncate split a multi-byte UTF-8 sequence: %s", w.Body.String())
	}
	var resp admin.DLQAnalyticsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(resp.TopErrors) != 1 {
		t.Fatalf("expected 1 top error, got %+v", resp.TopErrors)
	}
	if !utf8.ValidString(resp.TopErrors[0].ErrorText) {
		t.Fatalf("truncated error text is not valid UTF-8: %q", resp.TopErrors[0].ErrorText)
	}
	// 200 runes + the trailing ellipsis.
	if got := utf8.RuneCountInString(resp.TopErrors[0].ErrorText); got != 201 {
		t.Fatalf("expected 201 runes (200 + ellipsis), got %d (%q)", got, resp.TopErrors[0].ErrorText)
	}
}
