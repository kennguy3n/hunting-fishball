package audit_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type emitRecord struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (r *emitRecord) emit(_ context.Context, l *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, l)
	return nil
}

func seedAudit(t *testing.T, repo *audit.Repository, tenant string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		log := &audit.AuditLog{
			ID:           ulid.Make().String(),
			TenantID:     tenant,
			ActorID:      "user",
			Action:       audit.ActionRetrievalQueried,
			ResourceType: "retrieval",
			ResourceID:   "r",
			CreatedAt:    time.Now().UTC(),
		}
		if err := repo.Create(context.Background(), log); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func TestIntegrityWorker_BootstrapDoesNotFlag(t *testing.T) {

	db := newAuditDB(t)
	repo := audit.NewRepository(db)
	seedAudit(t, repo, "tenant-1")

	rec := &emitRecord{}
	w, err := audit.NewIntegrityWorker(audit.IntegrityWorkerConfig{
		Repo: repo,
		Tenants: func(_ context.Context) ([]string, error) {
			return []string{"tenant-1"}, nil
		},
		Interval: time.Second,
		EmitFn:   rec.emit,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mismatches, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if mismatches != 0 {
		t.Fatalf("bootstrap mismatches=%d want 0", mismatches)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 0 {
		t.Fatalf("expected no emissions on bootstrap")
	}
}

func TestIntegrityWorker_AppendDoesNotFlag(t *testing.T) {

	db := newAuditDB(t)
	repo := audit.NewRepository(db)
	seedAudit(t, repo, "tenant-1")

	rec := &emitRecord{}
	w, err := audit.NewIntegrityWorker(audit.IntegrityWorkerConfig{
		Repo: repo,
		Tenants: func(_ context.Context) ([]string, error) {
			return []string{"tenant-1"}, nil
		},
		EmitFn: rec.emit,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Append a new row (legitimate growth).
	seedAudit(t, repo, "tenant-1")
	mismatches, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if mismatches != 0 {
		t.Fatalf("append run mismatches=%d want 0", mismatches)
	}
}

// TestIntegrityWorker_AppendBeyondPageSize_DoesNotFlag is the
// Round-14 PR review regression. With the previous
// implementation tailMatches() re-fetched only the newest
// maxPageSize (=200) rows, so a chain that grew past 200 rows
// between two sweeps would lose its oldest rows from the window
// and the head hash could not be reproduced — guaranteeing a
// spurious "integrity violation". The fix paginates over the
// previously-observed (FirstEntry, LastEntry) range so the
// historical prefix is recomputed verbatim.
func TestIntegrityWorker_AppendBeyondPageSize_DoesNotFlag(t *testing.T) {
	db := newAuditDB(t)
	repo := audit.NewRepository(db)

	// Seed 250 rows (above the maxPageSize=200 cap).
	for i := 0; i < 250; i++ {
		log := &audit.AuditLog{
			ID:           ulid.Make().String(),
			TenantID:     "tenant-1",
			ActorID:      "user",
			Action:       audit.ActionRetrievalQueried,
			ResourceType: "retrieval",
			ResourceID:   "r",
			CreatedAt:    time.Now().UTC(),
		}
		if err := repo.Create(context.Background(), log); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rec := &emitRecord{}
	w, err := audit.NewIntegrityWorker(audit.IntegrityWorkerConfig{
		Repo: repo,
		Tenants: func(_ context.Context) ([]string, error) {
			return []string{"tenant-1"}, nil
		},
		EmitFn: rec.emit,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Append rows so the most-recent-200 window shifts older
	// rows out of view. Append a meaningful chunk so the new
	// head's window is fully disjoint from the previous head's
	// observed FirstEntry.
	for i := 0; i < 220; i++ {
		log := &audit.AuditLog{
			ID:           ulid.Make().String(),
			TenantID:     "tenant-1",
			ActorID:      "user",
			Action:       audit.ActionRetrievalQueried,
			ResourceType: "retrieval",
			ResourceID:   "r",
			CreatedAt:    time.Now().UTC(),
		}
		if err := repo.Create(context.Background(), log); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	mismatches, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if mismatches != 0 {
		t.Fatalf("append-past-pagesize mismatches=%d want 0 (paginated tailMatches regression)", mismatches)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 0 {
		t.Fatalf("expected no integrity-violation emissions, got %d", len(rec.logs))
	}
}

func TestIntegrityWorker_TamperFlagsViolation(t *testing.T) {

	db := newAuditDB(t)
	repo := audit.NewRepository(db)
	seedAudit(t, repo, "tenant-1")

	rec := &emitRecord{}
	w, err := audit.NewIntegrityWorker(audit.IntegrityWorkerConfig{
		Repo: repo,
		Tenants: func(_ context.Context) ([]string, error) {
			return []string{"tenant-1"}, nil
		},
		EmitFn: rec.emit,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Tamper: rewrite the metadata column on the oldest row.
	if err := db.Exec("UPDATE audit_logs SET metadata = ? WHERE tenant_id = ?", `{"tampered": true}`, "tenant-1").Error; err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// The integrity hash folds in (id, tenant_id, actor_id,
	// action, resource_type, resource_id) — metadata is NOT in
	// the chain. So this metadata change should NOT trip the
	// detector. Delete a row instead — that *will* change the
	// head.
	if err := db.Exec("DELETE FROM audit_logs WHERE tenant_id = ? AND id = (SELECT id FROM audit_logs WHERE tenant_id = ? ORDER BY id ASC LIMIT 1)", "tenant-1", "tenant-1").Error; err != nil {
		t.Fatalf("delete: %v", err)
	}
	mismatches, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if mismatches != 1 {
		t.Fatalf("mismatches=%d want 1", mismatches)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.logs) != 1 {
		t.Fatalf("expected one violation event, got %d", len(rec.logs))
	}
	if rec.logs[0].Action != audit.ActionAuditIntegrityViolation {
		t.Fatalf("action=%q", rec.logs[0].Action)
	}
}

func TestIntegrityCheckEnabled(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK", "")
	if audit.IntegrityCheckEnabled() {
		t.Fatalf("unset should be disabled")
	}
	t.Setenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK", "true")
	if !audit.IntegrityCheckEnabled() {
		t.Fatalf("true should be enabled")
	}
	t.Setenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK", "0")
	if audit.IntegrityCheckEnabled() {
		t.Fatalf("0 should be disabled")
	}
}

func TestIntegrityCheckInterval(t *testing.T) {
	t.Setenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL", "")
	if got := audit.IntegrityCheckInterval(); got != time.Hour {
		t.Fatalf("default=%v want 1h", got)
	}
	t.Setenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL", "5m")
	if got := audit.IntegrityCheckInterval(); got != 5*time.Minute {
		t.Fatalf("got=%v want 5m", got)
	}
}
