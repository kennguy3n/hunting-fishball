package regression_test

// round1213_manifest_test.go — Round-14 Task 9 meta-tests.
//
// Mirrors round911_manifest_test.go but scoped to
// Round1213Manifest. The meta-test asserts every named TestRef
// names an existing source file containing the expected
// `func TestName(` literal.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

func TestRound1213ManifestCoversPR22(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	for _, b := range regression.Round1213Manifest {
		got[b.PR]++
	}
	if got["22"] < 5 {
		t.Errorf("PR #22: covers %d bugs; want >= 5", got["22"])
	}
}

func TestRound1213ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round1213Manifest {
		for _, ref := range b.Tests {
			path := filepath.Join(root, ref.Source)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("PR #%s/%d: source %s missing: %v", b.PR, b.Finding, ref.Source, err)
				continue
			}
			needle := "func " + ref.TestName + "("
			if !strings.Contains(string(data), needle) {
				t.Errorf("PR #%s/%d: %s missing function %q", b.PR, b.Finding, ref.Source, needle)
			}
		}
	}
}
