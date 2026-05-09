package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

type fakeVS struct {
	hits []storage.QdrantHit
}

func (f *fakeVS) Search(_ context.Context, _ string, _ []float32, _ storage.SearchOpts) ([]storage.QdrantHit, error) {
	return f.hits, nil
}

type fakeEmb struct{ vec []float32 }

func (f *fakeEmb) EmbedQuery(_ context.Context, _, _ string) ([]float32, error) { return f.vec, nil }

type fakeBM25 struct{ hits []*retrieval.Match }

func (f *fakeBM25) Search(_ context.Context, _ string, _ string, _ int) ([]*retrieval.Match, error) {
	return f.hits, nil
}

// BenchmarkRetrieval_Latency measures end-to-end POST /v1/retrieve
// latency across vector + BM25 fan-out, recording P50/P95/P99 in
// microseconds. We seed each backend with deterministic synthetic
// hits so we're benchmarking the handler glue (errgroup, merger,
// reranker, policy filter) — not the storage layer.
func BenchmarkRetrieval_Latency(b *testing.B) {
	gin.SetMode(gin.TestMode)
	vs := &fakeVS{hits: synthVectorHits(20)}
	bm := &fakeBM25{hits: synthBM25Hits(20)}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: vs,
		Embedder:    &fakeEmb{vec: []float32{0.1, 0.2, 0.3}},
		BM25:        bm,
	})
	if err != nil {
		b.Fatalf("NewHandler: %v", err)
	}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "bench-tenant"); c.Next() })
	h.Register(&r.RouterGroup)

	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "context engine", PrivacyMode: "secret"})

	durations := make([]time.Duration, 0, b.N)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		t0 := time.Now()
		r.ServeHTTP(w, req)
		durations = append(durations, time.Since(t0))
		if w.Code != http.StatusOK {
			b.Fatalf("status: %d body: %s", w.Code, w.Body.String())
		}
	}
	b.StopTimer()

	if len(durations) == 0 {
		return
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	b.ReportMetric(float64(durations[percentileIdx(len(durations), 50)].Microseconds()), "p50_us")
	b.ReportMetric(float64(durations[percentileIdx(len(durations), 95)].Microseconds()), "p95_us")
	b.ReportMetric(float64(durations[percentileIdx(len(durations), 99)].Microseconds()), "p99_us")
}

func synthVectorHits(n int) []storage.QdrantHit {
	out := make([]storage.QdrantHit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, storage.QdrantHit{
			ID:    "v-" + iToS(i),
			Score: 1.0 / float32(i+1),
			Payload: map[string]any{
				"tenant_id":     "bench-tenant",
				"document_id":   "doc-" + iToS(i),
				"title":         "v title",
				"text":          "v text",
				"privacy_label": "internal",
			},
		})
	}

	return out
}

func synthBM25Hits(n int) []*retrieval.Match {
	out := make([]*retrieval.Match, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &retrieval.Match{
			ID:           "bm-" + iToS(i),
			Source:       retrieval.SourceBM25,
			Score:        float32(n - i),
			Rank:         i + 1,
			TenantID:     "bench-tenant",
			Title:        "bm title",
			Text:         "bm text",
			PrivacyLabel: "internal",
		})
	}

	return out
}

// percentileIdx returns the 0-based index of the p'th percentile in
// a slice of length n. p ∈ [0,100].
func percentileIdx(n, p int) int {
	idx := (n*p + 99) / 100
	if idx >= n {
		idx = n - 1
	}
	if idx < 0 {
		idx = 0
	}

	return idx
}
