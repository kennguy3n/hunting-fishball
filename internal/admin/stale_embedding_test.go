package admin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeChunkStore struct {
	mu       sync.Mutex
	rows     []*ChunkEmbeddingVersion
	failures map[string]string
	listErr  error
}

func newFakeChunkStore(rows ...*ChunkEmbeddingVersion) *fakeChunkStore {
	return &fakeChunkStore{rows: rows, failures: map[string]string{}}
}

func (f *fakeChunkStore) MarkStale(_ context.Context, tenantID, currentModelID string, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.TenantID == tenantID && r.EmbeddingModelID != currentModelID && r.StaleSince == nil {
			t := now
			r.StaleSince = &t
			n++
		}
	}

	return n, nil
}

func (f *fakeChunkStore) ListStale(_ context.Context, tenantID string, limit int) ([]*ChunkEmbeddingVersion, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*ChunkEmbeddingVersion{}
	for _, r := range f.rows {
		if r.TenantID == tenantID && r.StaleSince != nil {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}

	return out, nil
}

func (f *fakeChunkStore) RecordReembed(_ context.Context, row *ChunkEmbeddingVersion, modelID string, dim int, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.TenantID == row.TenantID && r.ChunkID == row.ChunkID {
			r.EmbeddingModelID = modelID
			r.EmbeddingDimensions = dim
			r.EmbeddedAt = now
			r.StaleSince = nil
			r.LastError = ""

			return nil
		}
	}

	return errors.New("not found")
}

func (f *fakeChunkStore) RecordFailure(_ context.Context, row *ChunkEmbeddingVersion, errMsg string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.TenantID == row.TenantID && r.ChunkID == row.ChunkID {
			r.Attempts++
			r.LastError = errMsg
			t := now
			r.LastAttemptAt = &t

			return nil
		}
	}

	return errors.New("not found")
}

type fakeCfgGetter struct {
	cfg *SourceEmbeddingConfig
	err error
}

func (f *fakeCfgGetter) Get(_ context.Context, _, _ string) (*SourceEmbeddingConfig, error) {
	return f.cfg, f.err
}

func TestStaleEmbeddingDetector_MarksDivergentRows(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newFakeChunkStore(
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c1", EmbeddingModelID: "old"},
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c2", EmbeddingModelID: "new"},
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c3", EmbeddingModelID: "old"},
	)
	det := NewStaleEmbeddingDetector(store, &fakeCfgGetter{
		cfg: &SourceEmbeddingConfig{ModelName: "new"},
	})
	got, err := det.DetectForSource(context.Background(), "t1", "s1", now)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != 2 {
		t.Fatalf("expected 2 rows marked, got %d", got)
	}
	if store.rows[1].StaleSince != nil {
		t.Fatalf("c2 (already on new) must not be marked")
	}
}

func TestStaleEmbeddingDetector_NoConfigIsNoop(t *testing.T) {
	store := newFakeChunkStore(&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c1", EmbeddingModelID: "old"})
	det := NewStaleEmbeddingDetector(store, &fakeCfgGetter{err: ErrEmbeddingConfigNotFound})
	got, err := det.DetectForSource(context.Background(), "t1", "s1", time.Now())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != 0 {
		t.Fatalf("expected no marking, got %d", got)
	}
}

func TestStaleEmbeddingWorker_DrainsRows(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleT := now.Add(-time.Hour)
	store := newFakeChunkStore(
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c1", EmbeddingModelID: "old", StaleSince: &staleT},
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c2", EmbeddingModelID: "old", StaleSince: &staleT},
	)
	calls := 0
	w := NewStaleEmbeddingWorker(StaleEmbeddingWorkerConfig{
		Store: store,
		ReEmbed: func(_ context.Context, _ *ChunkEmbeddingVersion) (string, int, error) {
			calls++

			return "new", 384, nil
		},
		Tenants: func(_ context.Context) ([]string, error) { return []string{"t1"}, nil },
		Now:     func() time.Time { return now },
	})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected re-embed called twice, got %d", calls)
	}
	for _, r := range store.rows {
		if r.StaleSince != nil {
			t.Fatalf("row %s still stale: %+v", r.ChunkID, r)
		}
		if r.EmbeddingModelID != "new" {
			t.Fatalf("row %s not updated: %+v", r.ChunkID, r)
		}
	}
}

func TestStaleEmbeddingWorker_RecordsFailure(t *testing.T) {
	now := time.Now()
	staleT := now.Add(-time.Hour)
	store := newFakeChunkStore(
		&ChunkEmbeddingVersion{TenantID: "t1", ChunkID: "c1", EmbeddingModelID: "old", StaleSince: &staleT},
	)
	w := NewStaleEmbeddingWorker(StaleEmbeddingWorkerConfig{
		Store: store,
		ReEmbed: func(_ context.Context, _ *ChunkEmbeddingVersion) (string, int, error) {
			return "", 0, errors.New("sidecar busy")
		},
		Tenants: func(_ context.Context) ([]string, error) { return []string{"t1"}, nil },
		Now:     func() time.Time { return now },
	})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if store.rows[0].StaleSince == nil {
		t.Fatalf("row should remain stale after failure")
	}
	if store.rows[0].Attempts != 1 || store.rows[0].LastError == "" {
		t.Fatalf("attempts/error not recorded: %+v", store.rows[0])
	}
}
