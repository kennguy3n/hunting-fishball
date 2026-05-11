package pipeline

// retry_analytics.go — Round-6 Task 19.
//
// Tracks retry counts, failure reasons, and success-after-retry
// rates per pipeline stage. Stages call RecordAttempt every time a
// piece of work runs; the aggregator keeps running counters and
// emits Prometheus metrics
// (`context_engine_pipeline_retries_total{stage,outcome}`) so
// alerting/dashboards can light up when a stage is wedged.
//
// `GET /v1/admin/pipeline/retry-stats` exposes the snapshot to
// operators.

import (
	"errors"
	"sort"
	"sync"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// RetryOutcome enumerates the result of a single attempt.
type RetryOutcome string

const (
	// RetryOutcomeSuccess — attempt completed without error.
	RetryOutcomeSuccess RetryOutcome = "success"
	// RetryOutcomeRetry — attempt failed and will be retried.
	RetryOutcomeRetry RetryOutcome = "retry"
	// RetryOutcomeFailed — attempt failed terminally (max retries
	// exceeded or unrecoverable error).
	RetryOutcomeFailed RetryOutcome = "failed"
)

// StageRetryStats is the per-stage aggregate.
type StageRetryStats struct {
	Stage            string         `json:"stage"`
	Attempts         int            `json:"attempts"`
	Successes        int            `json:"successes"`
	Retries          int            `json:"retries"`
	Failures         int            `json:"failures"`
	SuccessAfterRtry int            `json:"success_after_retry"`
	FailureReasons   map[string]int `json:"failure_reasons,omitempty"`
}

// RetryAnalytics is the in-process aggregator wired into every
// stage. It is concurrency-safe.
type RetryAnalytics struct {
	mu     sync.Mutex
	stages map[string]*StageRetryStats
	// retriesByDoc counts the number of retries seen for an
	// (stage, doc) pair before a success — used to compute the
	// "success_after_retry" rate.
	retriesByDoc map[string]int
}

// NewRetryAnalytics constructs an empty aggregator.
func NewRetryAnalytics() *RetryAnalytics {
	return &RetryAnalytics{
		stages:       map[string]*StageRetryStats{},
		retriesByDoc: map[string]int{},
	}
}

// RecordAttempt updates counters for a single attempt. docKey is
// any opaque key that uniquely identifies the work item so the
// aggregator can tell "first attempt then success" from "first
// attempt failed, second attempt succeeded". Reason can be empty
// for successes.
func (a *RetryAnalytics) RecordAttempt(stage, docKey string, outcome RetryOutcome, reason string) error {
	if stage == "" {
		return errors.New("retry analytics: missing stage")
	}
	if outcome == "" {
		return errors.New("retry analytics: missing outcome")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.stages[stage]
	if !ok {
		st = &StageRetryStats{Stage: stage, FailureReasons: map[string]int{}}
		a.stages[stage] = st
	}
	st.Attempts++
	key := stage + ":" + docKey
	switch outcome {
	case RetryOutcomeSuccess:
		st.Successes++
		if a.retriesByDoc[key] > 0 {
			st.SuccessAfterRtry++
		}
		delete(a.retriesByDoc, key)
	case RetryOutcomeRetry:
		st.Retries++
		a.retriesByDoc[key]++
		if reason != "" {
			st.FailureReasons[reason]++
		}
	case RetryOutcomeFailed:
		st.Failures++
		if reason != "" {
			st.FailureReasons[reason]++
		}
		delete(a.retriesByDoc, key)
	}
	observability.PipelineRetriesTotal.WithLabelValues(stage, string(outcome)).Inc()
	return nil
}

// Snapshot returns a deterministic, sorted copy of the per-stage
// stats. Safe to call from any goroutine.
func (a *RetryAnalytics) Snapshot() []*StageRetryStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*StageRetryStats, 0, len(a.stages))
	for _, st := range a.stages {
		cp := *st
		cp.FailureReasons = map[string]int{}
		for k, v := range st.FailureReasons {
			cp.FailureReasons[k] = v
		}
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
}
