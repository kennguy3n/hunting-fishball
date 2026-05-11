//go:build integration

// query_expansion_test.go — Round-10 Task 9.
//
// Drives the full wiring chain
//
//	SynonymStoreGORM (Postgres / SQLite) → SynonymExpander
//	  → retrieval.Handler.SetQueryExpander → BM25 fan-out
//
// to prove that a retrieve request containing a term with seeded
// synonyms reaches the BM25 backend with the *expanded* query
// string. This is the only place that exercises the runtime
// hand-off between the admin-owned synonym table and the
// retrieval handler's expander port, which is otherwise covered
// only by unit tests against the in-memory store.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// queryCapturingBM25 records every query string passed to Search
// so the test can assert on the expanded form. The hits payload
// is intentionally empty — the test cares about the *input* the
// backend saw, not the output it returned.
type queryCapturingBM25 struct {
	mu      sync.Mutex
	queries []string
	calls   atomic.Int32
}

func (q *queryCapturingBM25) Search(_ context.Context, _ string, query string, _ int) ([]*retrieval.Match, error) {
	q.calls.Add(1)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = append(q.queries, query)
	return nil, nil
}

// stubVectorStore returns no hits. The handler is wired with it
// only so the fan-out has a non-nil vector backend; the test does
// not assert on the vector path.
type stubVectorStore struct{}

func (stubVectorStore) Search(_ context.Context, _ string, _ []float32, _ storage.SearchOpts) ([]storage.QdrantHit, error) {
	return nil, nil
}

// stubEmbedder returns a fixed embedding. The vector backend is
// stubbed out so its value doesn't matter — but the handler
// still calls Embed before fanning out.
type stubEmbedder struct{}

func (stubEmbedder) EmbedQuery(_ context.Context, _, _ string) ([]float32, error) {
	return []float32{1}, nil
}

// retrieve_synonyms.sql column types are coerced to TEXT in
// SQLite; the GORM AutoMigrate handles this.
const synonymsSchemaSQLite = `CREATE TABLE retrieval_synonyms (
	tenant_id TEXT NOT NULL,
	term TEXT NOT NULL,
	synonyms TEXT NOT NULL,
	updated_at DATETIME NOT NULL,
	PRIMARY KEY (tenant_id, term)
);`

func TestQueryExpansion_EndToEnd_BM25SeesExpandedQuery(t *testing.T) {
	// Boot a SQLite GORM session and seed the synonyms table via
	// the production GORM store path. AutoMigrate runs once so
	// schema drift between the SQL migration and the row struct
	// would surface here.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	store, err := retrieval.NewSynonymStoreGORM(db)
	if err != nil {
		t.Fatalf("NewSynonymStoreGORM: %v", err)
	}
	if err := store.AutoMigrate(context.Background()); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := store.Set(context.Background(), "tenant-a", map[string][]string{
		"car": {"vehicle", "automobile"},
	}); err != nil {
		t.Fatalf("Set synonyms: %v", err)
	}

	// Wire the handler with the synonym expander pointed at the
	// production GORM store, mirroring what cmd/api builds.
	bm := &queryCapturingBM25{}
	h, err := retrieval.NewHandler(retrieval.HandlerConfig{
		VectorStore: stubVectorStore{},
		Embedder:    stubEmbedder{},
		BM25:        bm,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	h.SetQueryExpander(retrieval.NewSynonymExpander(store, 4))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	h.Register(&r.RouterGroup)

	body, _ := json.Marshal(retrieval.RetrieveRequest{Query: "car", TopK: 5, PrivacyMode: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/v1/retrieve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/v1/retrieve status: %d body: %s", w.Code, w.Body.String())
	}

	// The BM25 backend should have received the expanded form
	// "car vehicle automobile" rather than the raw "car". The
	// SynonymExpander preserves the original token first.
	if bm.calls.Load() == 0 {
		t.Fatalf("BM25 backend never invoked")
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if len(bm.queries) == 0 {
		t.Fatalf("no queries recorded by BM25")
	}
	got := bm.queries[0]
	for _, want := range []string{"car", "vehicle", "automobile"} {
		if !strings.Contains(got, want) {
			t.Fatalf("BM25 query %q missing %q", got, want)
		}
	}
}
