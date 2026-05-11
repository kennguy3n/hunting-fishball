package pipeline

// sync_history_hook.go — Round-10 Task 3.
//
// Coordinator-side per-(tenant, source) sync-run state and the
// callbacks the stage workers invoke. Keeping this in its own
// file avoids ballooning coordinator.go further.

import (
	"context"
	"sync/atomic"

	"github.com/oklog/ulid/v2"
)

// syncRunState tracks the docs_processed / docs_failed counters
// for one (tenant, source) backfill run between Start and Finish.
type syncRunState struct {
	runID     string
	processed atomic.Int64
	failed    atomic.Int64
}

func defaultSyncRunIDGen() string { return ulid.Make().String() }

// syncRunKey is the map key used by Coordinator.syncRuns. Keeping
// the construction central makes accidental key drift impossible.
func syncRunKey(tenantID, sourceID string) string {
	return tenantID + ":" + sourceID
}

// recordSyncStart inserts a `running` row when a backfill kickoff
// event enters Stage 1. Subsequent recordSyncOutcome calls bump
// counters; FinishBackfillRun closes the row.
func (c *Coordinator) recordSyncStart(ctx context.Context, evt IngestEvent) {
	if c.cfg.SyncHistory == nil || !IsKickoffEvent(evt) {
		return
	}
	runID := c.cfg.SyncRunIDGen()
	key := syncRunKey(evt.TenantID, evt.SourceID)

	c.syncRunsMu.Lock()
	// A re-emitted kickoff for the same (tenant, source) closes
	// the previous run as completed before opening a new one. The
	// admin store accepts the late Finish because the previous
	// run's row already exists.
	prev, hadPrev := c.syncRuns[key]
	c.syncRuns[key] = &syncRunState{runID: runID}
	c.syncRunsMu.Unlock()

	if hadPrev {
		_ = c.cfg.SyncHistory.Finish(ctx, evt.TenantID, evt.SourceID, prev.runID,
			SyncStatusSucceeded, int(prev.processed.Load()), int(prev.failed.Load()))
	}
	_ = c.cfg.SyncHistory.Start(ctx, evt.TenantID, evt.SourceID, runID)
}

// recordSyncOutcome bumps the per-run counter when a backfill
// document event finishes Stage 4 (or is routed to the DLQ).
// Non-backfill events and kickoff events themselves are ignored —
// the kickoff has no document payload to count.
func (c *Coordinator) recordSyncOutcome(evt IngestEvent, success bool) {
	if c.cfg.SyncHistory == nil || evt.SyncMode != SyncModeBackfill || IsKickoffEvent(evt) {
		return
	}
	key := syncRunKey(evt.TenantID, evt.SourceID)
	c.syncRunsMu.Lock()
	state := c.syncRuns[key]
	c.syncRunsMu.Unlock()
	if state == nil {
		return
	}
	if success {
		state.processed.Add(1)
	} else {
		state.failed.Add(1)
	}
}

// FinishBackfillRun closes the open sync_history row for
// (tenantID, sourceID). The caller decides the final status — the
// BackfillCompletionEmitter writes SyncStatusSucceeded when the
// progress endpoint hits 100%; the consumer shutdown path writes
// SyncStatusFailed when the pipeline is torn down with an open run.
//
// Returns nil when no run is open for the key (idempotent, safe to
// call from completion notifiers).
func (c *Coordinator) FinishBackfillRun(ctx context.Context, tenantID, sourceID string, status SyncStatus) error {
	if c.cfg.SyncHistory == nil {
		return nil
	}
	key := syncRunKey(tenantID, sourceID)
	c.syncRunsMu.Lock()
	state, ok := c.syncRuns[key]
	if ok {
		delete(c.syncRuns, key)
	}
	c.syncRunsMu.Unlock()
	if !ok {
		return nil
	}
	return c.cfg.SyncHistory.Finish(ctx, tenantID, sourceID, state.runID, status,
		int(state.processed.Load()), int(state.failed.Load()))
}
