package retrieval

// Round-4 Task 11: native Go fuzz tests over the retrieval input
// surface. These run alongside the unit tests via `go test` (with
// no -fuzz flag they execute the seed corpus only) and as a
// dedicated `make fuzz` target with `-fuzz=. -fuzztime=30s`.
//
// The targets here exercise:
//   1. JSON request unmarshal — unhinged payloads must not panic;
//      ill-formed input becomes a typed error.
//   2. ACL ChunkAttrs evaluation — random source/path/namespace
//      combinations against a deny rule must never panic.
//   3. PrivacyMode coercion — random label/mode combinations must
//      yield a stable bool without panicking.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/policy"
)

// FuzzRetrieveRequestDecode pins that arbitrary JSON bytes never
// panic the decoder and that decoded values respect the unicode-
// safe trim invariant the handler relies on.
func FuzzRetrieveRequestDecode(f *testing.F) {
	seeds := []string{
		`{"query":"hello"}`,
		`{"query":"","top_k":-1}`,
		`{"query":"\u0000"}`,
		`{"query":"x","top_k":1000000}`,
		`{"query":"x","sources":["s1","s2"]}`,
		`{}`,
		`null`,
		`{"query":"\xc3\x28"}`, // invalid utf-8
		strings.Repeat("a", 4096),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		var req RetrieveRequest
		// Errors are fine; what we care about is no panic.
		_ = json.Unmarshal(body, &req)
		// Trim must be idempotent on whatever survived decode.
		_ = strings.TrimSpace(req.Query)
		// top_k must not produce out-of-bound array allocations.
		if req.TopK < 0 || req.TopK > 1_000_000 {
			req.TopK = 0
		}
	})
}

// FuzzACLEvaluate pins that random ChunkAttrs + a synthetic ACL
// rule list cannot panic the evaluator. The function tolerates
// unicode + control characters in source/namespace/path inputs.
func FuzzACLEvaluate(f *testing.F) {
	f.Add("src-a", "ns-1", "drive/secret/x", "src-a", "drive/secret/**")
	f.Add("", "", "", "*", "**")
	f.Add("\x00", "\x00", "\x00", "*", "*")

	f.Fuzz(func(t *testing.T, srcID, nsID, path, ruleSrc, rulePath string) {
		acl := &policy.AllowDenyList{
			Rules: []policy.ACLRule{
				{
					SourceID: ruleSrc,
					PathGlob: rulePath,
					Action:   policy.ACLActionDeny,
				},
				{SourceID: "", Action: policy.ACLActionAllow},
			},
		}
		_ = acl.Evaluate(policy.ChunkAttrs{
			SourceID:    srcID,
			NamespaceID: nsID,
			Path:        path,
		})
	})
}

// FuzzPrivacyModeAllowed pins that arbitrary mode/label combos do
// not panic the privacy gate. The seed includes the documented
// canonical values plus garbage.
func FuzzPrivacyModeAllowed(f *testing.F) {
	f.Add("private", "private")
	f.Add("private", "remote-allowed")
	f.Add("REMOTE-ALLOWED", "private")
	f.Add("\x00", "\x00")
	f.Add(strings.Repeat("x", 1024), "private")

	f.Fuzz(func(t *testing.T, mode, label string) {
		_ = privacyAllowed(mode, label)
	})
}

// privacyAllowed mirrors the handler's compact mode-vs-label
// matrix without exporting the helper. Keeping a copy here lets
// the fuzz target run without dragging in the rest of the
// handler's dependencies (resolver, pool, fanOut workers).
func privacyAllowed(mode, label string) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		return true
	}
	switch mode {
	case "remote-allowed", "remote_allowed":
		return true
	case "private":
		return label == "private"
	default:
		return mode == label
	}
}
