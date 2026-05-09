package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

// fakeVectorStore implements VectorStore for tests.
type fakeVectorStore struct {
	mu          sync.Mutex
	collections map[string]bool
	upserts     []storage.QdrantPoint
	deleted     []string
	upsertErr   error
}

func (f *fakeVectorStore) EnsureCollection(_ context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.collections == nil {
		f.collections = map[string]bool{}
	}
	f.collections[tenantID] = true

	return nil
}

func (f *fakeVectorStore) Upsert(_ context.Context, _ string, points []storage.QdrantPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.upserts = append(f.upserts, points...)

	return nil
}

func (f *fakeVectorStore) Delete(_ context.Context, _ string, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, ids...)

	return nil
}

// fakeMetadataStore implements MetadataStore for tests.
type fakeMetadataStore struct {
	mu        sync.Mutex
	chunks    map[string]storage.Chunk
	upsertErr error
}

func newFakeMetadataStore() *fakeMetadataStore {
	return &fakeMetadataStore{chunks: map[string]storage.Chunk{}}
}

func (f *fakeMetadataStore) UpsertChunks(_ context.Context, tenantID string, chunks []storage.Chunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	for _, c := range chunks {
		if c.TenantID != tenantID {
			return errors.New("tenant mismatch")
		}
		f.chunks[c.ID] = c
	}

	return nil
}

func (f *fakeMetadataStore) LatestHashForDocument(_ context.Context, tenantID, documentID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.chunks {
		if c.TenantID == tenantID && c.DocumentID == documentID {
			return c.ContentHash, nil
		}
	}

	return "", nil
}

func (f *fakeMetadataStore) DeleteByDocument(_ context.Context, tenantID, documentID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, c := range f.chunks {
		if c.TenantID == tenantID && c.DocumentID == documentID {
			ids = append(ids, id)
			delete(f.chunks, id)
		}
	}

	return ids, nil
}

func newStorer(t *testing.T) (*Storer, *fakeVectorStore, *fakeMetadataStore) {
	t.Helper()
	v := &fakeVectorStore{}
	m := newFakeMetadataStore()
	s, err := NewStorer(StoreConfig{Vector: v, Metadata: m, Connector: "google_drive"})
	if err != nil {
		t.Fatalf("NewStorer: %v", err)
	}

	return s, v, m
}

func TestStorer_Store_Happy(t *testing.T) {
	t.Parallel()

	s, v, m := newStorer(t)
	doc := &Document{
		TenantID: "t", SourceID: "src", DocumentID: "doc-1",
		Title: "Hello", ContentHash: "h1", PrivacyLabel: "remote",
		Metadata: map[string]string{"uri": "https://x"},
	}
	blocks := []Block{
		{BlockID: "b1", Text: "hello"},
		{BlockID: "b2", Text: "world"},
	}
	emb := [][]float32{{1, 2}, {3, 4}}

	if err := s.Store(context.Background(), doc, blocks, emb, "model-v1"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if !v.collections["t"] {
		t.Fatal("collection not ensured")
	}
	if len(v.upserts) != 2 {
		t.Fatalf("upserts: %d", len(v.upserts))
	}
	for _, p := range v.upserts {
		if p.Payload["tenant_id"] == "t" {
			t.Fatal("storage layer must add tenant_id; pipeline must not pre-populate")
		}
		if p.Payload["privacy_label"] != "remote" {
			t.Fatalf("privacy_label not propagated: %+v", p.Payload)
		}
	}
	if len(m.chunks) != 2 {
		t.Fatalf("metadata chunks: %d", len(m.chunks))
	}
	for _, c := range m.chunks {
		if c.URI != "https://x" {
			t.Fatalf("uri: %q", c.URI)
		}
		if c.Connector != "google_drive" {
			t.Fatalf("connector: %q", c.Connector)
		}
	}
}

func TestStorer_Store_IdempotentSameHash(t *testing.T) {
	t.Parallel()

	s, _, m := newStorer(t)
	doc := &Document{TenantID: "t", DocumentID: "doc", ContentHash: "h"}
	blocks := []Block{{BlockID: "b1", Text: "x"}}
	emb := [][]float32{{1}}

	if err := s.Store(context.Background(), doc, blocks, emb, ""); err != nil {
		t.Fatalf("Store 1: %v", err)
	}
	first := len(m.chunks)

	if err := s.Store(context.Background(), doc, blocks, emb, ""); err != nil {
		t.Fatalf("Store 2: %v", err)
	}
	if len(m.chunks) != first {
		t.Fatalf("idempotent: %d vs %d", first, len(m.chunks))
	}
}

func TestStorer_Store_HashChangeReplacesPriorChunks(t *testing.T) {
	t.Parallel()

	s, v, m := newStorer(t)
	doc := &Document{TenantID: "t", DocumentID: "doc", ContentHash: "h1"}
	blocks := []Block{{BlockID: "b1", Text: "old"}}
	emb := [][]float32{{1}}

	if err := s.Store(context.Background(), doc, blocks, emb, ""); err != nil {
		t.Fatalf("Store h1: %v", err)
	}

	doc2 := &Document{TenantID: "t", DocumentID: "doc", ContentHash: "h2"}
	blocks2 := []Block{{BlockID: "b1", Text: "new"}, {BlockID: "b2", Text: "fresh"}}
	emb2 := [][]float32{{2}, {3}}
	if err := s.Store(context.Background(), doc2, blocks2, emb2, ""); err != nil {
		t.Fatalf("Store h2: %v", err)
	}

	// Old chunk must be deleted (count=1) and new chunks (b1+b2) inserted.
	if len(v.deleted) != 1 {
		t.Fatalf("expected 1 deleted, got %d", len(v.deleted))
	}
	if len(m.chunks) != 2 {
		t.Fatalf("expected 2 chunks after replace, got %d", len(m.chunks))
	}
}

func TestStorer_Store_MismatchedBlocksAndEmbeddings(t *testing.T) {
	t.Parallel()

	s, _, _ := newStorer(t)
	err := s.Store(context.Background(), &Document{TenantID: "t", DocumentID: "d"},
		[]Block{{BlockID: "b1"}}, [][]float32{{1}, {2}}, "")
	if err == nil {
		t.Fatal("expected mismatch error")
	}
}

func TestStorer_Store_PoisonOnMissingTenant(t *testing.T) {
	t.Parallel()

	s, _, _ := newStorer(t)
	err := s.Store(context.Background(), &Document{DocumentID: "d"}, nil, nil, "")
	if !errors.Is(err, ErrPoisonMessage) {
		t.Fatalf("expected ErrPoisonMessage: %v", err)
	}
}

func TestStorer_Delete(t *testing.T) {
	t.Parallel()

	s, v, m := newStorer(t)
	doc := &Document{TenantID: "t", DocumentID: "doc", ContentHash: "h"}
	if err := s.Store(context.Background(), doc, []Block{{BlockID: "b1", Text: "x"}}, [][]float32{{1}}, ""); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := s.Delete(context.Background(), "t", "doc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(m.chunks) != 0 {
		t.Fatalf("metadata not cleared: %d", len(m.chunks))
	}
	if len(v.deleted) != 1 {
		t.Fatalf("vector delete not called")
	}
}

func TestStorer_NewStorer_Validation(t *testing.T) {
	t.Parallel()

	if _, err := NewStorer(StoreConfig{}); err == nil {
		t.Fatal("expected error for nil VectorStore")
	}
	if _, err := NewStorer(StoreConfig{Vector: &fakeVectorStore{}}); err == nil {
		t.Fatal("expected error for nil MetadataStore")
	}
}
