package shard_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

type recordSweeper struct {
	calls    []string
	failOnce bool
	failed   bool
}

func (r *recordSweeper) ForgetTenant(_ context.Context, tenantID string) error {
	r.calls = append(r.calls, tenantID)
	if r.failOnce && !r.failed {
		r.failed = true
		return errors.New("transient")
	}
	return nil
}

type capturingAudit struct {
	logs []*audit.AuditLog
}

func (c *capturingAudit) Create(_ context.Context, log *audit.AuditLog) error {
	c.logs = append(c.logs, log)
	return nil
}

func newRedisClient(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func TestForget_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	rc, _ := newRedisClient(t)

	sw1 := &recordSweeper{}
	sw2 := &recordSweeper{}
	auditW := &capturingAudit{}

	f, err := shard.NewForget(shard.ForgetConfig{
		Repo:     repo,
		Sweepers: []shard.TenantSweeper{sw1, sw2},
		Lease:    rc,
		Audit:    auditW,
	})
	if err != nil {
		t.Fatalf("new forget: %v", err)
	}

	if err := f.Forget(context.Background(), "tenant-a", "admin-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if len(sw1.calls) != 1 || len(sw2.calls) != 1 {
		t.Fatalf("sweepers not called: %v %v", sw1.calls, sw2.calls)
	}
	state, _ := repo.LifecycleState(context.Background(), "tenant-a")
	if state != shard.LifecycleDeleted {
		t.Fatalf("state: %q", state)
	}
	if len(auditW.logs) != 2 {
		t.Fatalf("expected 2 audit logs, got %d", len(auditW.logs))
	}
	if string(auditW.logs[0].Action) != "tenant.deletion_requested" {
		t.Fatalf("first audit action: %q", auditW.logs[0].Action)
	}
	if string(auditW.logs[1].Action) != "tenant.deleted" {
		t.Fatalf("second audit action: %q", auditW.logs[1].Action)
	}
}

func TestForget_LeaseHeld(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	rc, mr := newRedisClient(t)

	// Pre-acquire the lease so Forget can't.
	if err := mr.Set("hf:forget:tenant:tenant-a", "other-token"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f, err := shard.NewForget(shard.ForgetConfig{
		Repo:     repo,
		Sweepers: []shard.TenantSweeper{&recordSweeper{}},
		Lease:    rc,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	err = f.Forget(context.Background(), "tenant-a", "admin-1")
	if !errors.Is(err, shard.ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld, got %v", err)
	}
}

func TestForget_SweeperErrorLeavesPending(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	rc, _ := newRedisClient(t)

	sw := &recordSweeper{failOnce: true}
	f, err := shard.NewForget(shard.ForgetConfig{
		Repo:     repo,
		Sweepers: []shard.TenantSweeper{sw},
		Lease:    rc,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	err = f.Forget(context.Background(), "tenant-a", "admin-1")
	if err == nil {
		t.Fatal("expected error from sweeper")
	}
	state, _ := repo.LifecycleState(context.Background(), "tenant-a")
	if state != shard.LifecyclePendingDeletion {
		t.Fatalf("state should remain pending_deletion, got %q", state)
	}
}

func TestForget_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	rc, _ := newRedisClient(t)
	f, err := shard.NewForget(shard.ForgetConfig{
		Repo:  newTestRepo(t),
		Lease: rc,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := f.Forget(context.Background(), "", "admin"); !errors.Is(err, shard.ErrMissingTenantScope) {
		t.Fatalf("want missing-tenant, got %v", err)
	}
}

// Drain duration is exercised here via a tiny non-zero value to keep
// the test fast. The orchestrator must not skip the drain even when
// the sweeper list is empty.
func TestForget_DrainBeforeSweep(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	rc, _ := newRedisClient(t)

	f, err := shard.NewForget(shard.ForgetConfig{
		Repo:          repo,
		Lease:         rc,
		DrainDuration: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	start := time.Now()
	if err := f.Forget(context.Background(), "tenant-a", "admin"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Fatalf("drain skipped: %v", time.Since(start))
	}
}
