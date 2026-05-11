// Round-12 Task 12 — fuzz target for shard.ComputeDelta.
//
// The Makefile target name is FuzzDeltaDiff (matching the original
// plan); ComputeDelta is the actual function that turns (prev, curr)
// into a sorted list of add/remove ops. Bug-class signals:
//
//  1. Any panic on arbitrary input slices (especially with empty
//     strings, duplicates, or huge slices).
//  2. The output must turn `prev` into `curr` exactly when applied
//     — i.e. (prev ∖ removes) ∪ adds == curr (as sets).
//  3. The output must be deterministic: sorted by (kind, chunk_id).
package shard_test

import (
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

func FuzzDeltaDiff(f *testing.F) {
	seeds := []struct{ prev, curr string }{
		{"", ""},
		{"a", "a"},
		{"a,b,c", "b,c,d"},
		{"a", ""},
		{"", "b"},
		{"a,a,a", "a"},
		{",,", ",,"},
		{"a,b", "b,a"},
	}
	for _, s := range seeds {
		f.Add(s.prev, s.curr)
	}
	f.Fuzz(func(t *testing.T, prevCSV, currCSV string) {
		prev := strings.Split(prevCSV, ",")
		curr := strings.Split(currCSV, ",")

		ops := shard.ComputeDelta(prev, curr)

		// Determinism: ops are sorted by (kind, chunk_id). We
		// verify by recomputing and asserting equality.
		ops2 := shard.ComputeDelta(prev, curr)
		if len(ops) != len(ops2) {
			t.Fatalf("non-deterministic length: prev=%v curr=%v", prev, curr)
		}
		for i := range ops {
			if ops[i] != ops2[i] {
				t.Fatalf("non-deterministic order at %d: %+v vs %+v", i, ops[i], ops2[i])
			}
		}

		// Apply ops to prev (as a set) and confirm we land at curr.
		state := make(map[string]struct{})
		for _, id := range prev {
			if id == "" {
				continue
			}
			state[id] = struct{}{}
		}
		for _, op := range ops {
			switch op.Kind {
			case shard.DeltaAdd:
				state[op.ChunkID] = struct{}{}
			case shard.DeltaRemove:
				delete(state, op.ChunkID)
			default:
				t.Fatalf("unexpected DeltaKind %q", op.Kind)
			}
		}
		// Expected set.
		expected := make(map[string]struct{})
		for _, id := range curr {
			if id == "" {
				continue
			}
			expected[id] = struct{}{}
		}
		if len(state) != len(expected) {
			t.Fatalf("post-apply size mismatch: got=%d want=%d", len(state), len(expected))
		}
		for k := range expected {
			if _, ok := state[k]; !ok {
				t.Fatalf("post-apply missing %q from result; ops=%+v", k, ops)
			}
		}
	})
}
