// Round-12 Task 6 — DLQAutoReplayer unit tests.
package pipeline_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

type memAutoStore struct{ rows []pipeline.DLQMessage }

func (m *memAutoStore) ListAutoReplayable(_ context.Context, now time.Time, limit, maxAutoRetries int) ([]pipeline.DLQMessage, error) {
	if maxAutoRetries <= 0 {
		maxAutoRetries = pipeline.DefaultAutoReplayMaxRetries
	}
	out := make([]pipeline.DLQMessage, 0, len(m.rows))
	cut := now.Add(-pipeline.AutoReplayBackoff[0])
	for _, r := range m.rows {
		if r.ReplayedAt != nil {
			continue
		}
		if r.AttemptCount >= maxAutoRetries {
			continue
		}
		if r.FailedAt.After(cut) {
			continue
		}
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memAutoStore) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	for i := range m.rows {
		if m.rows[i].TenantID == tenantID && m.rows[i].ID == id {
			cp := m.rows[i]
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

type recordingAutoReplayer struct {
	calls   []string
	failOn  string
	bumpFn  func(id string)
	maxAttn int
}

func (r *recordingAutoReplayer) Replay(_ context.Context, tenantID, id, topic string, _ bool) error {
	r.calls = append(r.calls, tenantID+":"+id+":"+topic)
	if r.failOn == id {
		return errors.New("simulated failure")
	}
	if r.bumpFn != nil {
		r.bumpFn(id)
	}
	return nil
}
func (r *recordingAutoReplayer) MaxAttempts() int {
	if r.maxAttn == 0 {
		return 5
	}
	return r.maxAttn
}

func TestDLQAutoReplay_RepliesEligibleRow_IncrementsSuccessCounter(t *testing.T) {
	observability.DLQAutoReplaysTotal.Reset()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &memAutoStore{rows: []pipeline.DLQMessage{
		{
			ID: "dlq-1", TenantID: "tenant-a", OriginalTopic: "ingest",
			AttemptCount: 0,
			FailedAt:     now.Add(-2 * time.Minute), // beyond 1m backoff
		},
	}}
	rec := &recordingAutoReplayer{}
	w, err := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store: store, Replayer: rec,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 replay, got %d", len(rec.calls))
	}
	if rec.calls[0] != "tenant-a:dlq-1:ingest" {
		t.Fatalf("call=%q", rec.calls[0])
	}
	if got := testutil.ToFloat64(observability.DLQAutoReplaysTotal.WithLabelValues("success")); got != 1 {
		t.Fatalf("success counter=%v", got)
	}
}

func TestDLQAutoReplay_SkipsRowsWithinBackoffWindow(t *testing.T) {
	observability.DLQAutoReplaysTotal.Reset()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &memAutoStore{rows: []pipeline.DLQMessage{
		{
			ID: "dlq-young", TenantID: "tenant-a", OriginalTopic: "ingest",
			AttemptCount: 0,
			FailedAt:     now.Add(-10 * time.Second), // inside 1m window
		},
	}}
	rec := &recordingAutoReplayer{}
	w, _ := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store: store, Replayer: rec,
		Now: func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 replays, got %d", len(rec.calls))
	}
}

func TestDLQAutoReplay_SkipsRowsAtMaxRetries(t *testing.T) {
	observability.DLQAutoReplaysTotal.Reset()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &memAutoStore{rows: []pipeline.DLQMessage{
		{
			ID: "dlq-burned", TenantID: "tenant-a", OriginalTopic: "ingest",
			AttemptCount: 3, // at retry budget
			FailedAt:     now.Add(-2 * time.Hour),
		},
	}}
	rec := &recordingAutoReplayer{}
	w, _ := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store: store, Replayer: rec,
		Now: func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 replays (burned), got %d", len(rec.calls))
	}
}

func TestDLQAutoReplay_FailureIncrementsFailureCounter(t *testing.T) {
	observability.DLQAutoReplaysTotal.Reset()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &memAutoStore{rows: []pipeline.DLQMessage{
		{
			ID: "dlq-flaky", TenantID: "tenant-a", OriginalTopic: "ingest",
			AttemptCount: 0,
			FailedAt:     now.Add(-2 * time.Minute),
		},
	}}
	rec := &recordingAutoReplayer{failOn: "dlq-flaky"}
	w, _ := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store: store, Replayer: rec,
		Now: func() time.Time { return now },
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := testutil.ToFloat64(observability.DLQAutoReplaysTotal.WithLabelValues("failure")); got != 1 {
		t.Fatalf("failure counter=%v", got)
	}
	if got := testutil.ToFloat64(observability.DLQAutoReplaysTotal.WithLabelValues("success")); got != 0 {
		t.Fatalf("success counter=%v (expected 0)", got)
	}
}

// Regression: prior to PR #21 follow-up, ListAutoReplayable
// hardcoded DefaultAutoReplayMaxRetries in the SQL filter, so
// raising cfg.MaxAutoRetries had no effect — rows with
// AttemptCount >= 3 were silently dropped. This asserts the budget
// is plumbed all the way through. Note that NewDLQAutoReplayer
// clamps MaxAutoRetries to len(AutoReplayBackoff) (currently 3) to
// keep the SQL filter and the in-memory BackoffFor consistent, so
// the meaningful regression case is a value < default.
func TestDLQAutoReplay_HonoursConfigurableMaxRetries(t *testing.T) {
	observability.DLQAutoReplaysTotal.Reset()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &memAutoStore{rows: []pipeline.DLQMessage{
		{
			ID: "dlq-attn-1", TenantID: "tenant-a", OriginalTopic: "ingest",
			AttemptCount: 1,
			FailedAt:     now.Add(-2 * time.Hour),
		},
	}}
	rec := &recordingAutoReplayer{}
	w, err := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store: store, Replayer: rec,
		MaxAutoRetries: 1, // tighter budget than default; row at 1 is now ineligible
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("expected 0 replays under tighter budget, got %d", len(rec.calls))
	}
}

func TestBackoffFor_ScheduleAndBudget(t *testing.T) {
	cases := []struct {
		n     int
		want  time.Duration
		valid bool
	}{
		{0, 1 * time.Minute, true},
		{1, 5 * time.Minute, true},
		{2, 30 * time.Minute, true},
		{3, 0, false},
		{-1, 0, false},
	}
	for _, c := range cases {
		got, ok := pipeline.BackoffFor(c.n)
		if ok != c.valid || got != c.want {
			t.Errorf("BackoffFor(%d)=(%v,%v) want (%v,%v)", c.n, got, ok, c.want, c.valid)
		}
	}
}
