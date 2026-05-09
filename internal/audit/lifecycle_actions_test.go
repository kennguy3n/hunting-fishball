package audit_test

import (
	"context"
	"sync"
	"testing"

	"github.com/IBM/sarama"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// lifecycleProducer is a tiny audit.MessageProducer-shaped fake used
// only by the lifecycle-action tests. The outbox-test fixture uses a
// different fake (sarama.ProducerMessage list with explicit error
// injection); duplicating a 5-line shim keeps the lifecycle tests
// independent of the broader outbox fixture's internals.
type lifecycleProducer struct {
	mu   sync.Mutex
	msgs []*sarama.ProducerMessage
}

func newFakeProducer() *lifecycleProducer { return &lifecycleProducer{} }

func (p *lifecycleProducer) SendMessage(msg *sarama.ProducerMessage) (int32, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, msg)
	return 0, int64(len(p.msgs) - 1), nil
}

func (p *lifecycleProducer) Close() error { return nil }

func (p *lifecycleProducer) actions() []audit.Action {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]audit.Action, 0, len(p.msgs))
	for _, m := range p.msgs {
		body, _ := m.Value.Encode()
		// Best-effort: extract the action substring rather than
		// re-importing JSON. The action key is unique enough that a
		// substring match is reliable for the canonical row shape.
		out = append(out, extractAction(string(body)))
	}
	return out
}

// extractAction pulls the value of the "action" JSON field from the
// audit row body. The body shape comes from internal/audit/model.go's
// AuditLog struct serialised with encoding/json.
func extractAction(s string) audit.Action {
	const key = `"action":"`
	i := indexOf(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	j := indexOf(rest, `"`)
	if j < 0 {
		return ""
	}
	return audit.Action(rest[:j])
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestLifecycleActions_Persist verifies the new Phase 2 / Phase 4
// connector lifecycle action constants round-trip through the
// repository unchanged. Without this guard a typo in the model.go
// constants (or a typo on the receiver side) would silently corrupt
// the admin filter dropdown.
func TestLifecycleActions_Persist(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()

	cases := []struct {
		name string
		want audit.Action
	}{
		{"connected", audit.ActionSourceConnected},
		{"paused", audit.ActionSourcePaused},
		{"resumed", audit.ActionSourceResumed},
		{"re_scoped", audit.ActionSourceReScoped},
		{"sync_started", audit.ActionSourceSyncStarted},
		{"chunk_indexed", audit.ActionChunkIndexed},
		{"purged", audit.ActionSourcePurged},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Cannot t.Parallel() here — the shared :memory:
			// SQLite DB is bound to the parent test's
			// connection pool. Sub-test parallelism would race
			// pool acquisitions that resolve to different
			// in-memory databases.
			log := audit.NewAuditLog("tenant-a", "actor-1", c.want, "source", "src-1", nil, "")
			if err := repo.Create(ctx, log); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := repo.Get(ctx, "tenant-a", log.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Action != c.want {
				t.Fatalf("Action mismatch: got=%q want=%q", got.Action, c.want)
			}
		})
	}
}

// TestLifecycleActions_OutboxPublishes verifies the outbox poller
// publishes the new lifecycle actions to Kafka unchanged. The
// transactional outbox is the source of truth for downstream
// observability — a regression here would silently swallow events.
func TestLifecycleActions_OutboxPublishes(t *testing.T) {
	t.Parallel()

	repo, _ := newSQLiteRepo(t)
	ctx := context.Background()
	for _, a := range []audit.Action{
		audit.ActionSourceConnected,
		audit.ActionSourcePaused,
		audit.ActionSourceResumed,
		audit.ActionSourceReScoped,
		audit.ActionSourceSyncStarted,
		audit.ActionChunkIndexed,
		audit.ActionSourcePurged,
	} {
		log := audit.NewAuditLog("tenant-a", "actor-1", a, "source", "src", nil, "")
		if err := repo.Create(ctx, log); err != nil {
			t.Fatalf("Create %s: %v", a, err)
		}
	}

	prod := newFakeProducer()
	ob, err := audit.NewOutbox(repo, prod, audit.OutboxConfig{Topic: "audit.events"})
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	if _, err := ob.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got := prod.actions()
	wantSet := map[audit.Action]bool{
		audit.ActionSourceConnected:   true,
		audit.ActionSourcePaused:      true,
		audit.ActionSourceResumed:     true,
		audit.ActionSourceReScoped:    true,
		audit.ActionSourceSyncStarted: true,
		audit.ActionChunkIndexed:      true,
		audit.ActionSourcePurged:      true,
	}
	for _, a := range got {
		if !wantSet[a] {
			t.Fatalf("unexpected action published: %q", a)
		}
		delete(wantSet, a)
	}
	if len(wantSet) != 0 {
		t.Fatalf("missing published actions: %v", wantSet)
	}
}
