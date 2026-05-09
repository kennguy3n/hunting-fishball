package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// fakeQdrant runs an httptest server that emulates the slice of the
// Qdrant REST API the QdrantClient exercises.
type fakeQdrant struct {
	mu          sync.Mutex
	collections map[string]bool
	// points keyed on collection → id → record
	points map[string]map[string]struct {
		Vector  []float32
		Payload map[string]any
	}
}

func newFakeQdrant() (*httptest.Server, *fakeQdrant) {
	q := &fakeQdrant{
		collections: map[string]bool{},
		points: map[string]map[string]struct {
			Vector  []float32
			Payload map[string]any
		}{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/collections/", func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()

		path := r.URL.Path[len("/collections/"):]
		// /collections/<name>
		if r.Method == http.MethodGet && !containsSubpath(path) {
			if q.collections[path] {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}

			return
		}
		if r.Method == http.MethodPut && !containsSubpath(path) {
			q.collections[path] = true
			if q.points[path] == nil {
				q.points[path] = map[string]struct {
					Vector  []float32
					Payload map[string]any
				}{}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

			return
		}
		// /collections/<name>/points
		if r.Method == http.MethodPut && hasSuffix(path, "/points") {
			col := path[:len(path)-len("/points")]
			if !q.collections[col] {
				w.WriteHeader(http.StatusNotFound)

				return
			}
			var body struct {
				Points []struct {
					ID      string         `json:"id"`
					Vector  []float32      `json:"vector"`
					Payload map[string]any `json:"payload"`
				} `json:"points"`
			}
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, p := range body.Points {
				q.points[col][p.ID] = struct {
					Vector  []float32
					Payload map[string]any
				}{Vector: p.Vector, Payload: p.Payload}
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

			return
		}
		// /collections/<name>/points/search
		if r.Method == http.MethodPost && hasSuffix(path, "/points/search") {
			col := path[:len(path)-len("/points/search")]
			pts := q.points[col]
			results := []map[string]any{}
			var body struct {
				Limit int `json:"limit"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			i := 0
			for id, p := range pts {
				results = append(results, map[string]any{
					"id":      id,
					"score":   1.0 - float32(i)*0.1,
					"payload": p.Payload,
				})
				i++
				if body.Limit > 0 && i >= body.Limit {
					break
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": results})

			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})

	srv := httptest.NewServer(mux)

	return srv, q
}

func containsSubpath(p string) bool { return len(splitFirst(p, '/')) > 1 }
func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}
func splitFirst(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}

	return []string{s}
}

func TestQdrant_RejectsEmptyTenant(t *testing.T) {
	t.Parallel()

	c, err := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: "http://x", VectorSize: 4})
	if err != nil {
		t.Fatalf("NewQdrantClient: %v", err)
	}
	if err := c.EnsureCollection(context.Background(), ""); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if err := c.Upsert(context.Background(), "", []storage.QdrantPoint{{ID: "x"}}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := c.Search(context.Background(), "", []float32{1, 2}, storage.SearchOpts{}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Search: %v", err)
	}
	if err := c.Delete(context.Background(), "", []string{"a"}); !errors.Is(err, storage.ErrMissingTenantScope) {
		t.Fatalf("Delete: %v", err)
	}
}

func TestQdrant_NewQdrantClient_Validation(t *testing.T) {
	t.Parallel()

	if _, err := storage.NewQdrantClient(storage.QdrantConfig{}); err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
	if _, err := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: "x"}); err == nil {
		t.Fatal("expected error for zero VectorSize")
	}
}

func TestQdrant_EnsureCollectionIdempotent(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeQdrant()
	defer srv.Close()
	c, err := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: srv.URL, VectorSize: 4, CollectionPrefix: "hf-"})
	if err != nil {
		t.Fatalf("NewQdrantClient: %v", err)
	}
	if err := c.EnsureCollection(context.Background(), "tenant-a"); err != nil {
		t.Fatalf("EnsureCollection 1: %v", err)
	}
	if err := c.EnsureCollection(context.Background(), "tenant-a"); err != nil {
		t.Fatalf("EnsureCollection 2: %v", err)
	}
	if got := c.CollectionFor("tenant-a"); got != "hf-tenant-a" {
		t.Fatalf("CollectionFor: %q", got)
	}
}

func TestQdrant_UpsertAndSearch(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeQdrant()
	defer srv.Close()
	c, _ := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: srv.URL, VectorSize: 3})
	ctx := context.Background()

	if err := c.EnsureCollection(ctx, "tenant-a"); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	err := c.Upsert(ctx, "tenant-a", []storage.QdrantPoint{
		{ID: "p1", Vector: []float32{1, 0, 0}, Payload: map[string]any{"text": "hello"}},
		{ID: "p2", Vector: []float32{0, 1, 0}, Payload: map[string]any{"text": "world"}},
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := c.Search(ctx, "tenant-a", []float32{1, 0, 0}, storage.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits: %d", len(hits))
	}

	// Confirm the fake stores the tenant_id in payload.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, p := range fake.points["tenant-a"] {
		if p.Payload["tenant_id"] != "tenant-a" {
			t.Fatalf("tenant_id missing from payload: %+v", p.Payload)
		}
	}
}

func TestQdrant_Search_RejectsEmptyVector(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeQdrant()
	defer srv.Close()
	c, _ := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: srv.URL, VectorSize: 1})
	if _, err := c.Search(context.Background(), "tenant-a", nil, storage.SearchOpts{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestQdrant_Upsert_NoOpOnEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeQdrant()
	defer srv.Close()
	c, _ := storage.NewQdrantClient(storage.QdrantConfig{BaseURL: srv.URL, VectorSize: 1})
	if err := c.Upsert(context.Background(), "t", nil); err != nil {
		t.Fatalf("Upsert empty: %v", err)
	}
}
