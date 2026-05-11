//go:build e2e

// Round-13 Task 14 — concurrent tenant deletion race test.
//
// Spawns two goroutines simultaneously calling Forget on the same
// tenant_id. The fenced Redis lease in shard.Forget must guarantee
// exactly one Forget call sweeps the storage tier; the other must
// observe shard.ErrLeaseHeld.
//
// This protects the cryptographic-forget orchestrator from
// double-free of DEKs and dangling shard rows when two replicas
// receive the same DELETE call (e.g. a retry storm during a node
// flap).
package e2e

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

const concurrentDeleteSQLiteSchema = `
CREATE TABLE shards (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    user_id       TEXT,
    channel_id    TEXT,
    privacy_mode  TEXT NOT NULL,
    shard_version INTEGER NOT NULL DEFAULT 1,
    chunks_count  INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_shards_tenant_cdt ON shards (tenant_id, user_id, channel_id);

CREATE TABLE shard_chunks (
    shard_id  TEXT NOT NULL,
    chunk_id  TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    PRIMARY KEY (shard_id, chunk_id)
);

CREATE TABLE tenant_lifecycle (
    tenant_id    TEXT PRIMARY KEY,
    state        TEXT NOT NULL,
    requested_by TEXT,
    requested_at DATETIME NOT NULL,
    deleted_at   DATETIME
);
`

type concurrentSweeper struct {
	calls atomic.Int64
}

func (c *concurrentSweeper) ForgetTenant(_ context.Context, _ string) error {
	c.calls.Add(1)
	return nil
}

type concurrentAudit struct {
	mu   sync.Mutex
	logs []*audit.AuditLog
}

func (c *concurrentAudit) Create(_ context.Context, l *audit.AuditLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *l
	c.logs = append(c.logs, &cp)
	return nil
}

func newConcurrentRepo(t *testing.T) *shard.Repository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	if sqlDB, derr := db.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.Exec(concurrentDeleteSQLiteSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return shard.NewRepository(db)
}

func TestConcurrentTenantDelete_OnlyOneSucceeds(t *testing.T) {
	repo := newConcurrentRepo(t)
	// Pre-seed the lifecycle row so both workers pass the
	// idempotent step-1 "mark pending" call without racing on the
	// unique constraint. The point of the test is that the
	// fenced lease in step 3 is the single race point — not the
	// upsert in step 1 (which uses ON CONFLICT in production).
	if err := repo.MarkLifecycle(context.Background(), "tenant-race",
		shard.LifecyclePendingDeletion, "seed"); err != nil {
		t.Fatalf("seed lifecycle: %v", err)
	}
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	sweeper := &concurrentSweeper{}
	auditor := &concurrentAudit{}

	mkForget := func() *shard.Forget {
		f, err := shard.NewForget(shard.ForgetConfig{
			Repo:     repo,
			Sweepers: []shard.TenantSweeper{sweeper},
			Lease:    rc,
			Audit:    auditor,
		})
		if err != nil {
			t.Fatalf("new forget: %v", err)
		}
		return f
	}

	const tenantID = "tenant-race"
	var wg sync.WaitGroup
	var successes, leaseHelds atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := mkForget().Forget(context.Background(), tenantID, "admin-1")
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, shard.ErrLeaseHeld):
				leaseHelds.Add(1)
			default:
				t.Errorf("worker %d unexpected err: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if successes.Load() != 1 {
		t.Fatalf("expected exactly 1 success, got %d (lease_held=%d)",
			successes.Load(), leaseHelds.Load())
	}
	if leaseHelds.Load() != 1 {
		t.Fatalf("expected exactly 1 lease_held, got %d (success=%d)",
			leaseHelds.Load(), successes.Load())
	}
	if sweeper.calls.Load() != 1 {
		t.Fatalf("expected sweeper called exactly once, got %d", sweeper.calls.Load())
	}
}
