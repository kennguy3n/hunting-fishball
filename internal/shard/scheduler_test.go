package shard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type stubLister struct {
	tenants []string
	err     error
}

func (s *stubLister) ActiveTenants(_ context.Context) ([]string, error) {
	return s.tenants, s.err
}

type stubManifests struct {
	rows map[string][]*ManifestSummary
	err  error
}

func (s *stubManifests) LatestByTenant(_ context.Context, tenantID string) ([]*ManifestSummary, error) {
	return s.rows[tenantID], s.err
}

type stubSubmitter struct {
	mu  sync.Mutex
	got []string
}

func (s *stubSubmitter) SubmitShardRequest(_ context.Context, tenantID, kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, tenantID+":"+kind)
	return nil
}

func (s *stubSubmitter) snap() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.got))
	copy(out, s.got)
	return out
}

func TestScheduler_Validation(t *testing.T) {
	cases := []SchedulerConfig{
		{Manifests: &stubManifests{}, Submitter: &stubSubmitter{}},
		{TenantLister: &stubLister{}, Submitter: &stubSubmitter{}},
		{TenantLister: &stubLister{}, Manifests: &stubManifests{}},
	}
	for i, tc := range cases {
		if _, err := NewScheduler(tc); err == nil {
			t.Fatalf("case %d should fail validation", i)
		}
	}
}

func TestScheduler_StaleShardSubmitted(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	manifests := &stubManifests{rows: map[string][]*ManifestSummary{
		"t1": {{TenantID: "t1", ShardKind: "base", UpdatedAt: now.Add(-48 * time.Hour)}},
	}}
	sub := &stubSubmitter{}
	sched, err := NewScheduler(SchedulerConfig{
		TenantLister:    &stubLister{tenants: []string{"t1"}},
		Manifests:       manifests,
		Submitter:       sub,
		FreshnessWindow: 24 * time.Hour,
		Tick:            time.Minute,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := sched.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	hits := sub.snap()
	if len(hits) != 1 || hits[0] != "t1:base" {
		t.Fatalf("unexpected hits: %v", hits)
	}
	if got := sched.Stats(); got.StaleDetected != 1 || got.Submitted != 1 || got.SkippedFresh != 0 {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

func TestScheduler_FreshShardSkipped(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	manifests := &stubManifests{rows: map[string][]*ManifestSummary{
		"t1": {{TenantID: "t1", ShardKind: "base", UpdatedAt: now.Add(-1 * time.Hour)}},
	}}
	sub := &stubSubmitter{}
	sched, _ := NewScheduler(SchedulerConfig{
		TenantLister:    &stubLister{tenants: []string{"t1"}},
		Manifests:       manifests,
		Submitter:       sub,
		FreshnessWindow: 24 * time.Hour,
		Now:             func() time.Time { return now },
	})
	_ = sched.RunOnce(context.Background())
	if hits := sub.snap(); len(hits) != 0 {
		t.Fatalf("expected no submission for fresh shard; got %v", hits)
	}
	if got := sched.Stats(); got.SkippedFresh != 1 {
		t.Fatalf("expected SkippedFresh=1; got %+v", got)
	}
}

func TestScheduler_MissingShardSubmitted(t *testing.T) {
	now := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	manifests := &stubManifests{rows: map[string][]*ManifestSummary{}}
	sub := &stubSubmitter{}
	sched, _ := NewScheduler(SchedulerConfig{
		TenantLister:    &stubLister{tenants: []string{"t1"}},
		Manifests:       manifests,
		Submitter:       sub,
		FreshnessWindow: 24 * time.Hour,
		Now:             func() time.Time { return now },
	})
	_ = sched.RunOnce(context.Background())
	if hits := sub.snap(); len(hits) != 1 {
		t.Fatalf("expected 1 hit for missing shard; got %v", hits)
	}
}

func TestScheduler_TenantListerError(t *testing.T) {
	sched, _ := NewScheduler(SchedulerConfig{
		TenantLister: &stubLister{err: errors.New("db down")},
		Manifests:    &stubManifests{},
		Submitter:    &stubSubmitter{},
	})
	if err := sched.RunOnce(context.Background()); err == nil {
		t.Fatalf("expected error from tenant lister")
	}
}

func TestScheduler_RunStopsOnContext(t *testing.T) {
	sched, _ := NewScheduler(SchedulerConfig{
		TenantLister: &stubLister{},
		Manifests:    &stubManifests{},
		Submitter:    &stubSubmitter{},
		Tick:         50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- sched.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Run did not return on cancel")
	}
}
