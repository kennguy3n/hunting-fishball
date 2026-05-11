package main

// round10_hooks.go — Round-10 Tasks 3 & 4 adapters.
//
// pipeline.{SyncHistoryRecorder, ChunkQualityRecorder} are narrow
// pipeline-side ports so the pipeline package never imports admin.
// cmd/ingest owns the only construction site where the GORM-backed
// admin stores get adapted to those ports.

import (
	"context"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// syncHistoryAdapter wraps an admin.SyncHistoryRecorder so it
// satisfies pipeline.SyncHistoryRecorder. Only the SyncStatus enum
// needs converting; the rest is one-to-one.
type syncHistoryAdapter struct {
	inner admin.SyncHistoryRecorder
}

func newSyncHistoryAdapter(inner admin.SyncHistoryRecorder) *syncHistoryAdapter {
	return &syncHistoryAdapter{inner: inner}
}

func (a *syncHistoryAdapter) Start(ctx context.Context, tenantID, sourceID, runID string) error {
	return a.inner.Start(ctx, tenantID, sourceID, runID)
}

func (a *syncHistoryAdapter) Finish(ctx context.Context, tenantID, sourceID, runID string, status pipeline.SyncStatus, processed, failed int) error {
	return a.inner.Finish(ctx, tenantID, sourceID, runID, admin.SyncStatus(status), processed, failed)
}

// chunkQualityAdapter wraps an admin.ChunkQualityStoreGORM so it
// satisfies pipeline.ChunkQualityRecorder.
type chunkQualityAdapter struct {
	inner *admin.ChunkQualityStoreGORM
}

func newChunkQualityAdapter(inner *admin.ChunkQualityStoreGORM) *chunkQualityAdapter {
	return &chunkQualityAdapter{inner: inner}
}

func (a *chunkQualityAdapter) Record(ctx context.Context, r pipeline.ChunkQualityReport) error {
	row := admin.ChunkQualityRow{
		TenantID:     r.TenantID,
		SourceID:     r.SourceID,
		DocumentID:   r.DocumentID,
		ChunkID:      r.ChunkID,
		QualityScore: r.QualityScore,
		LengthScore:  r.LengthScore,
		LangScore:    r.LangScore,
		EmbedScore:   r.EmbedScore,
		Duplicate:    r.Duplicate,
		UpdatedAt:    time.Now().UTC(),
	}
	return a.inner.Insert(ctx, row)
}

// makeRunID returns the default ULID generator the coordinator
// uses to mint sync_history.sync_run_id values. Wrapped here so a
// future override (e.g. a deterministic generator under test) can
// be swapped in cmd/ingest without touching the pipeline package.
func makeRunID() string { return ulid.Make().String() }
