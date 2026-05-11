package regression_test

// round911_manifest_test.go — Round-12 Task 18 meta-tests.
//
// Mirrors round910_manifest_test.go but scoped to Round911Manifest.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

// TestRound911ManifestCoversAllListedBugs confirms the manifest
// enumerates the PR-#20 findings catalogued from Round-11 Devin
// Review.
func TestRound911ManifestCoversAllListedBugs(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	for _, b := range regression.Round911Manifest {
		got[b.PR]++
	}
	// We catalogue six distinct Devin Review findings on PR #20.
	if got["20"] != 6 {
		t.Errorf("PR #20: covers %d bugs; want 6", got["20"])
	}
}

// TestRound911ManifestRefsExistInTree spot-checks that every
// TestRef in Round911Manifest names a file that exists and
// contains the named test/fuzz function.
func TestRound911ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round911Manifest {
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

// TestRound911ManifestNoEmptyFields rejects entries that landed
// with a missing field.
func TestRound911ManifestNoEmptyFields(t *testing.T) {
	t.Parallel()
	for i, b := range regression.Round911Manifest {
		if b.PR == "" || b.Finding == 0 || b.Title == "" || b.Symptom == "" || len(b.Tests) == 0 {
			t.Errorf("Round911Manifest[%d] has empty fields: %+v", i, b)
		}
		for j, ref := range b.Tests {
			if ref.PkgRel == "" || ref.TestName == "" || ref.Source == "" {
				t.Errorf("Round911Manifest[%d].Tests[%d] has empty fields: %+v", i, j, ref)
			}
		}
	}
}
