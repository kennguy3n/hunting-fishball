package pipeline

// sync_history.go — Round-10 Task 3.
//
// The pipeline coordinator emits sync_history rows so admin
// dashboards can show "which sources are syncing right now and
// which finished with errors". The interface defined here is
// implemented by internal/admin.SyncHistoryGORM (and the in-memory
// fake in tests); pipeline → admin imports stay forbidden because
// admin already depends on pipeline.
//
// Lifecycle:
//   - Stage 1 sees a backfill kickoff event (IsKickoffEvent) and
//     calls Start to insert a `running` row.
//   - Stage 4 / DLQ increments per-(tenant, source) processed and
//     failed counters as backfill events flow through.
//   - The owner of completion (the BackfillCompletionEmitter in
//     cmd/api, the consumer's shutdown path in cmd/ingest) calls
//     Coordinator.FinishBackfillRun to close the row with a final
//     SyncStatus and the accumulated counts.

import "context"

// SyncStatus mirrors admin.SyncStatus exactly so the recorder
// adapter in cmd/api / cmd/ingest is a one-line string conversion.
type SyncStatus string

const (
	// SyncStatusRunning marks a row that's still accumulating
	// counters. Inserted by Start. Mirrors admin.SyncStatusRunning.
	SyncStatusRunning SyncStatus = "running"
	// SyncStatusSucceeded marks a backfill that finished without
	// pipeline-level errors. Written by Finish. Mirrors
	// admin.SyncStatusSucceeded.
	SyncStatusSucceeded SyncStatus = "succeeded"
	// SyncStatusFailed marks a backfill that hit a pipeline-level
	// failure (DLQ exhausted retries, completion timeout, etc.).
	// Written by Finish. Mirrors admin.SyncStatusFailed.
	SyncStatusFailed SyncStatus = "failed"
)

// SyncHistoryRecorder is the narrow contract the coordinator
// needs to persist a row per backfill run. The signature is the
// minimal subset of admin.SyncHistoryRecorder; cmd/api / cmd/ingest
// wraps the GORM-backed concrete store at construction time.
type SyncHistoryRecorder interface {
	Start(ctx context.Context, tenantID, sourceID, runID string) error
	Finish(ctx context.Context, tenantID, sourceID, runID string, status SyncStatus, processed, failed int) error
}
