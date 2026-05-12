package retrieval

import "testing"

// FuzzSlowQueryThreshold — Round-14 Task 11.
//
// Fuzzes ParseSlowQueryThreshold against arbitrary string input.
// The parser MUST return (0, false) on any malformed input; the
// fuzzer drives the assertion by checking that a successful
// parse never yields a negative integer (the catastrophic case
// that would let an attacker disable slow-query logging via env
// injection).
func FuzzSlowQueryThreshold(f *testing.F) {
	f.Add("1000")
	f.Add("0")
	f.Add("")
	f.Add("   ")
	f.Add("-1")
	f.Add("9999999999")
	f.Add("abc")
	f.Add(" 500 ")
	f.Fuzz(func(t *testing.T, raw string) {
		v, ok := ParseSlowQueryThreshold(raw)
		if ok && v < 0 {
			t.Fatalf("ParseSlowQueryThreshold(%q) returned (%d, true) which is invariant-breaking", raw, v)
		}
		if !ok && v != 0 {
			t.Fatalf("ParseSlowQueryThreshold(%q) returned (%d, false); v should be 0 on failure", raw, v)
		}
	})
}
