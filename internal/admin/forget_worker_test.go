package admin_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// recordingSweeper captures (tenant, source) pairs forwarded to it.
type recordingSweeper struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingSweeper) ForgetSource(_ context.Context, tenant, source string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, tenant+"/"+source)
	return nil
}

func (r *recordingSweeper) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newForgetWorker(t *testing.T, sweepers []admin.ForgetSweeper, ad admin.AuditWriter) (*admin.ForgetWorker, *admin.SourceRepository, *redis.Client) {
	t.Helper()
	repo, _ := newSQLiteSourceRepo(t)
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	w, err := admin.NewForgetWorker(admin.ForgetWorkerConfig{
		Repo:     repo,
		Sweepers: sweepers,
		Lease:    rc,
		LeaseTTL: time.Minute,
		Audit:    ad,
	})
	if err != nil {
		t.Fatalf("NewForgetWorker: %v", err)
	}
	return w, repo, rc
}

func seedRemoving(t *testing.T, repo *admin.SourceRepository, tenant, connectorType string) string {
	t.Helper()
	src := admin.NewSource(tenant, connectorType, nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.MarkRemoving(context.Background(), tenant, src.ID); err != nil {
		t.Fatalf("MarkRemoving: %v", err)
	}
	return src.ID
}

func TestForgetWorker_RunsAllSweepersAndMarksRemoved(t *testing.T) {
	t.Parallel()
	sw1 := &recordingSweeper{}
	sw2 := &recordingSweeper{}
	ad := &fakeAudit{}
	w, repo, _ := newForgetWorker(t, []admin.ForgetSweeper{sw1, sw2}, ad)
	srcID := seedRemoving(t, repo, "tenant-a", "google-drive")

	if err := w.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: srcID, Actor: "actor-1"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sw1.snapshot(); len(got) != 1 || got[0] != "tenant-a/"+srcID {
		t.Fatalf("sw1 calls: %v", got)
	}
	if got := sw2.snapshot(); len(got) != 1 {
		t.Fatalf("sw2 calls: %v", got)
	}
	got, _ := repo.Get(context.Background(), "tenant-a", srcID)
	if got.Status != admin.SourceStatusRemoved {
		t.Fatalf("expected status removed, got %q", got.Status)
	}
	if a := ad.actions(); len(a) != 1 || a[0] != audit.ActionSourcePurged {
		t.Fatalf("audit actions: %v", a)
	}
}

func TestForgetWorker_LeaseBlocksConcurrentRuns(t *testing.T) {
	t.Parallel()
	// Block sweeper #1 inside the first goroutine so the lease is
	// held when the second goroutine attempts the same job.
	gate := make(chan struct{})
	first := admin.SweeperFunc(func(_ context.Context, _, _ string) error {
		<-gate
		return nil
	})
	noop := admin.SweeperFunc(func(_ context.Context, _, _ string) error { return nil })
	w, repo, rc := newForgetWorker(t, []admin.ForgetSweeper{first}, &fakeAudit{})
	srcID := seedRemoving(t, repo, "tenant-a", "slack")

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: srcID}) }()

	// Give goroutine 1 time to acquire the lease.
	time.Sleep(50 * time.Millisecond)

	w2, _ := admin.NewForgetWorker(admin.ForgetWorkerConfig{
		Repo:     repo,
		Sweepers: []admin.ForgetSweeper{noop},
		Lease:    rc,
		LeaseTTL: time.Minute,
	})
	err := w2.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: srcID})
	if !errors.Is(err, admin.ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld, got %v", err)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
}

func TestForgetWorker_RefusesIfStatusNotRemoving(t *testing.T) {
	t.Parallel()
	w, repo, _ := newForgetWorker(t, []admin.ForgetSweeper{&recordingSweeper{}}, &fakeAudit{})
	src := admin.NewSource("tenant-a", "google-drive", nil, nil)
	if err := repo.Create(context.Background(), src); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Status is `active` — worker must refuse.
	if err := w.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: src.ID}); err == nil {
		t.Fatal("expected refusal because status is not removing")
	}
}

func TestForgetWorker_SweeperErrorAbortsAndPreservesStatus(t *testing.T) {
	t.Parallel()
	bad := &recordingSweeper{err: errors.New("qdrant 500")}
	good := &recordingSweeper{}
	w, repo, _ := newForgetWorker(t, []admin.ForgetSweeper{good, bad}, &fakeAudit{})
	srcID := seedRemoving(t, repo, "tenant-a", "google-drive")

	if err := w.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: srcID}); err == nil {
		t.Fatal("expected sweeper error to propagate")
	}
	got, _ := repo.Get(context.Background(), "tenant-a", srcID)
	if got.Status != admin.SourceStatusRemoving {
		t.Fatalf("status must stay removing for retry, got %q", got.Status)
	}
}

func TestForgetWorker_RejectsMissingIdentifiers(t *testing.T) {
	t.Parallel()
	w, _, _ := newForgetWorker(t, nil, &fakeAudit{})
	if err := w.Run(context.Background(), admin.ForgetJob{}); err == nil {
		t.Fatal("expected error on empty job")
	}
}

// TestForgetWorker_ReleaseDoesNotEvictForeignLease guards against a
// stale unconditional DEL eating a freshly-acquired lease. We pause
// the worker mid-run, overwrite the lease key to simulate the case
// where this worker's TTL expired and a different worker took over,
// then let the original finish. Its deferred release MUST honor the
// stored token (compare-and-delete) and leave the foreign lease
// untouched.
func TestForgetWorker_ReleaseDoesNotEvictForeignLease(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	blocker := admin.SweeperFunc(func(_ context.Context, _, _ string) error {
		<-gate
		return nil
	})
	w, repo, rc := newForgetWorker(t, []admin.ForgetSweeper{blocker}, &fakeAudit{})
	srcID := seedRemoving(t, repo, "tenant-a", "google-drive")

	done := make(chan error, 1)
	go func() {
		done <- w.Run(context.Background(), admin.ForgetJob{TenantID: "tenant-a", SourceID: srcID})
	}()

	leaseKey := "hf:forget:tenant-a:" + srcID
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, err := rc.Get(context.Background(), leaseKey).Result()
		if err == nil && v != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	const stolen = "stolen-by-other-worker"
	if err := rc.Set(context.Background(), leaseKey, stolen, time.Minute).Err(); err != nil {
		t.Fatalf("Set: %v", err)
	}

	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := rc.Get(context.Background(), leaseKey).Result()
	if err != nil {
		t.Fatalf("foreign lease was evicted by stale release: %v", err)
	}
	if got != stolen {
		t.Fatalf("foreign lease overwritten: %q", got)
	}
}
