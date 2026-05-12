package pipeline_test

// dlq_category_test.go — Round-14 Task 14.
//
// Asserts that
//   (1) CategoriseDLQError maps representative error messages to
//       the correct category.
//   (2) The auto-replay worker skips DLQCategoryPermanent rows.

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestCategoriseDLQError_TableDriven(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", pipeline.DLQCategoryUnknown},
		{"context deadline exceeded", pipeline.DLQCategoryTransient},
		{"i/o timeout", pipeline.DLQCategoryTransient},
		{"connection refused", pipeline.DLQCategoryTransient},
		{"http 503 unavailable", pipeline.DLQCategoryTransient},
		{"got 429 from upstream", pipeline.DLQCategoryTransient},
		{"parse error at offset 3", pipeline.DLQCategoryPermanent},
		{"schema violation: missing tenant_id", pipeline.DLQCategoryPermanent},
		{"json decode failed", pipeline.DLQCategoryPermanent},
		{"poison message detected", pipeline.DLQCategoryPermanent},
		{"some other surface", pipeline.DLQCategoryUnknown},
	}
	for _, c := range cases {
		if got := pipeline.CategoriseDLQError(c.in); got != c.want {
			t.Errorf("CategoriseDLQError(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

// autoReplayFakeStore captures Replay calls and serves a fixed
// list of rows from ListAutoReplayable.
type autoReplayFakeStore struct {
	rows []pipeline.DLQMessage
}

func (s *autoReplayFakeStore) ListAutoReplayable(_ context.Context, _ time.Time, _, _ int) ([]pipeline.DLQMessage, error) {
	return s.rows, nil
}

func (s *autoReplayFakeStore) Get(_ context.Context, tenantID, id string) (*pipeline.DLQMessage, error) {
	for i := range s.rows {
		if s.rows[i].TenantID == tenantID && s.rows[i].ID == id {
			cp := s.rows[i]
			return &cp, nil
		}
	}
	return nil, errors.New("not found")
}

type recordingReplayer struct {
	ids []string
}

func (r *recordingReplayer) Replay(_ context.Context, _, id, _ string, _ bool) error {
	r.ids = append(r.ids, id)
	return nil
}

func (r *recordingReplayer) MaxAttempts() int { return 5 }

func TestAutoReplayer_SkipsPermanentRows(t *testing.T) {
	rows := []pipeline.DLQMessage{
		{ID: "permanent-1", TenantID: "t", OriginalTopic: "x", Category: pipeline.DLQCategoryPermanent, FailedAt: time.Now().Add(-time.Hour)},
		{ID: "transient-1", TenantID: "t", OriginalTopic: "x", Category: pipeline.DLQCategoryTransient, FailedAt: time.Now().Add(-time.Hour)},
		{ID: "unknown-1", TenantID: "t", OriginalTopic: "x", Category: pipeline.DLQCategoryUnknown, FailedAt: time.Now().Add(-time.Hour)},
	}
	store := &autoReplayFakeStore{rows: rows}
	replayer := &recordingReplayer{}
	w, err := pipeline.NewDLQAutoReplayer(pipeline.DLQAutoReplayConfig{
		Store:    store,
		Replayer: replayer,
		Logger:   slog.Default(),
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := w.Tick(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("tick: %v", err)
	}
	// Permanent row must NOT appear in replayer.ids.
	for _, id := range replayer.ids {
		if id == "permanent-1" {
			t.Fatalf("permanent row was replayed: %v", replayer.ids)
		}
	}
	// At least the transient row must have been attempted (the
	// unknown legacy row also replays for compat).
	var sawTransient bool
	for _, id := range replayer.ids {
		if id == "transient-1" {
			sawTransient = true
		}
	}
	if !sawTransient {
		t.Fatalf("transient row was not replayed: %v", replayer.ids)
	}
}
