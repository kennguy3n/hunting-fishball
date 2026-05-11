// Round-12 Task 12 — fuzz target for ParsePartitionKey.
//
// The partition-key parser splits a "tenant||source" string. A
// malformed input must never panic; bug-class signals are:
//
//  1. Panics on weird Unicode, control bytes, or huge inputs.
//  2. A successful parse with empty tenant or empty source.
//  3. A round-trip from PartitionKey(parsedTenant, parsedSource)
//     that doesn't equal the original input — meaning the
//     forward composer and reverse parser disagree.
package pipeline_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

func FuzzParsePartitionKey(f *testing.F) {
	seeds := []string{
		"",
		"||",
		"tenant||source",
		"tenant|source",
		"||source",
		"tenant||",
		"a||b||c",
		"tenant\x00||source",
		"\xff||\xff",
		"\u0000\u0001||\u0002\u0003",
		"abcabcabcabc||" + string(make([]byte, 1024)),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		tenant, source, ok := pipeline.ParsePartitionKey(key)
		if !ok {
			return
		}
		if tenant == "" || source == "" {
			t.Fatalf("ParsePartitionKey reported ok with empty half: key=%q tenant=%q source=%q",
				key, tenant, source)
		}
		// Round-trip via the canonical composer. The detailed parse
		// flags legacy single-pipe keys, in which case the
		// composer's `||` output will differ from the original
		// input — that's expected, so skip the assertion when
		// legacy=true.
		t2, s2, legacy, _ := pipeline.ParsePartitionKeyDetailed(key)
		if legacy {
			return
		}
		composed := pipeline.PartitionKey(t2, s2)
		if composed != key {
			t.Fatalf("non-legacy round-trip mismatch: %q -> (%q,%q) -> %q",
				key, t2, s2, composed)
		}
	})
}
