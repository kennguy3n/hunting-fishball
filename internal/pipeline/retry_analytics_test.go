package pipeline_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func TestRetryAnalytics_RecordValidation(t *testing.T) {
	a := pipeline.NewRetryAnalytics()
	if err := a.RecordAttempt("", "k", pipeline.RetryOutcomeSuccess, ""); err == nil {
		t.Fatalf("empty stage should fail")
	}
	if err := a.RecordAttempt("fetch", "k", "", ""); err == nil {
		t.Fatalf("empty outcome should fail")
	}
}

func TestRetryAnalytics_SuccessAfterRetry(t *testing.T) {
	observability.ResetForTest()
	a := pipeline.NewRetryAnalytics()
	if err := a.RecordAttempt("embed", "doc-1", pipeline.RetryOutcomeRetry, "timeout"); err != nil {
		t.Fatalf("record retry: %v", err)
	}
	if err := a.RecordAttempt("embed", "doc-1", pipeline.RetryOutcomeRetry, "timeout"); err != nil {
		t.Fatalf("record retry: %v", err)
	}
	if err := a.RecordAttempt("embed", "doc-1", pipeline.RetryOutcomeSuccess, ""); err != nil {
		t.Fatalf("record success: %v", err)
	}
	snap := a.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 stage; got %d", len(snap))
	}
	st := snap[0]
	if st.Attempts != 3 || st.Successes != 1 || st.Retries != 2 {
		t.Fatalf("bad counts: %+v", st)
	}
	if st.SuccessAfterRtry != 1 {
		t.Fatalf("expected SuccessAfterRtry=1; got %d", st.SuccessAfterRtry)
	}
	if st.FailureReasons["timeout"] != 2 {
		t.Fatalf("expected timeout=2; got %d", st.FailureReasons["timeout"])
	}
}

func TestRetryAnalytics_TerminalFailure(t *testing.T) {
	observability.ResetForTest()
	a := pipeline.NewRetryAnalytics()
	_ = a.RecordAttempt("store", "doc-2", pipeline.RetryOutcomeRetry, "5xx")
	_ = a.RecordAttempt("store", "doc-2", pipeline.RetryOutcomeFailed, "5xx")
	snap := a.Snapshot()
	st := snap[0]
	if st.Successes != 0 || st.Failures != 1 {
		t.Fatalf("expected 1 failure; got %+v", st)
	}
	if st.SuccessAfterRtry != 0 {
		t.Fatalf("terminal failure must not bump SuccessAfterRtry")
	}
}

func TestRetryAnalytics_MultiStageSorted(t *testing.T) {
	observability.ResetForTest()
	a := pipeline.NewRetryAnalytics()
	for _, stage := range []string{"store", "parse", "fetch", "embed"} {
		_ = a.RecordAttempt(stage, "k", pipeline.RetryOutcomeSuccess, "")
	}
	snap := a.Snapshot()
	want := []string{"embed", "fetch", "parse", "store"}
	if len(snap) != len(want) {
		t.Fatalf("len=%d", len(snap))
	}
	for i, s := range snap {
		if s.Stage != want[i] {
			t.Fatalf("position %d stage=%s want %s", i, s.Stage, want[i])
		}
	}
}
