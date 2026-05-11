// Round-12 Task 12 — fuzz target for admin.QueryHash.
//
// QueryHash returns a stable sha256 prefix of the query text. Bug-
// class signals:
//
//  1. Any panic on arbitrary input strings (control bytes, huge
//     strings, invalid UTF-8 sequences).
//  2. Determinism: QueryHash(s) must equal QueryHash(s) on every
//     call.
//  3. Output shape: always exactly 64 lower-case hex characters
//     (sha256 hex digest).
package admin_test

import (
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func FuzzQueryHash(f *testing.F) {
	seeds := []string{
		"",
		"hello",
		strings.Repeat("a", 1024),
		"\x00\x01\x02",
		"\xff\xfe\xfd",
		"日本語",
		"emoji 🤔",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h := admin.QueryHash(s)
		if len(h) != 64 {
			t.Fatalf("len(hash)=%d, want 64; input=%q", len(h), s)
		}
		for i := 0; i < len(h); i++ {
			c := h[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("non-hex char %q at %d in %q (input=%q)", c, i, h, s)
			}
		}
		// Determinism: a second call returns the same value.
		if h2 := admin.QueryHash(s); h2 != h {
			t.Fatalf("non-deterministic hash: %q vs %q for input %q", h, h2, s)
		}
	})
}
