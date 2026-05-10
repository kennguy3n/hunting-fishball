package main

import "testing"

// TestStageWorkerEnv covers the env-var → int conversion that drives
// per-stage worker pool sizing in the coordinator. Empty / invalid
// values must round-trip to 0 so pipeline.StageConfig.defaults()
// applies its per-stage default of 1 and preserves the original
// single-goroutine semantics.
func TestStageWorkerEnv(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want int
	}{
		{"unset returns zero", "", 0},
		{"plain integer", "4", 4},
		{"single digit", "1", 1},
		{"zero is treated as default", "0", 0},
		{"negative is rejected", "-1", 0},
		{"non-numeric is rejected", "many", 0},
		{"trailing junk is rejected", "4x", 0},
		{"large value accepted", "128", 128},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STAGE_WORKER_TEST", tc.val)
			if tc.val == "" {
				if got := stageWorkerEnv("STAGE_WORKER_UNSET"); got != tc.want {
					t.Fatalf("got %d want %d", got, tc.want)
				}
				return
			}
			if got := stageWorkerEnv("STAGE_WORKER_TEST"); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
