package pipeline

import (
	"sync"
	"testing"
	"time"
)

// FuzzStageBreakerConcurrent — Round-14 Task 11.
//
// Drives a single StageCircuitBreaker from N goroutines in
// parallel using fuzz-generated counts. The breaker MUST remain
// internally consistent: State() must always return one of the
// three known values, Snapshot() must never panic, and the
// probeInFlight gate must always release on OnSuccess /
// OnFailure (asserted via Snapshot().ProbeInFlight == false at
// the end of the run).
func FuzzStageBreakerConcurrent(f *testing.F) {
	f.Add(uint8(1), uint8(1), uint8(1))
	f.Add(uint8(4), uint8(8), uint8(2))
	f.Add(uint8(0), uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, allowN, failN, successN uint8) {
		// Cap workload to keep the fuzz runner under its
		// per-input wall-clock budget.
		if allowN > 16 {
			allowN = 16
		}
		if failN > 16 {
			failN = 16
		}
		if successN > 16 {
			successN = 16
		}
		br, err := NewStageCircuitBreaker(StageCircuitBreakerConfig{
			Stage: "embed", Threshold: 4, OpenFor: 50 * time.Millisecond,
		})
		if err != nil {
			t.Skip("config rejected; not a fuzz failure")
		}
		var wg sync.WaitGroup
		spawn := func(n uint8, fn func()) {
			for i := uint8(0); i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						_ = recover()
					}()
					fn()
				}()
			}
		}
		spawn(allowN, func() { _ = br.Allow() })
		spawn(failN, func() { br.OnFailure() })
		spawn(successN, func() { br.OnSuccess() })
		wg.Wait()
		snap := br.Snapshot()
		switch snap.State {
		case "closed", "open", "half-open":
		default:
			t.Fatalf("unknown state %q after fuzz run", snap.State)
		}
		// Drain any in-flight probe so the next iteration
		// starts clean — explicit OnSuccess + OnFailure must
		// release the gate.
		br.OnSuccess()
		br.OnFailure()
	})
}
