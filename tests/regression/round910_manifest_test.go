package regression_test

// round910_manifest_test.go — Round-11 Task 3 meta-tests.
//
// Mirrors round78_manifest_test.go but scoped to Round910Manifest.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

// TestRound910ManifestCoversAllListedBugs confirms the manifest
// enumerates the expected counts: 3 PR-#18 findings + 3 PR-#19
// findings.
func TestRound910ManifestCoversAllListedBugs(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	for _, b := range regression.Round910Manifest {
		got[b.PR]++
	}
	if got["18"] != 3 {
		t.Errorf("PR #18: covers %d bugs; want 3", got["18"])
	}
	if got["19"] != 3 {
		t.Errorf("PR #19: covers %d bugs; want 3", got["19"])
	}
}

// TestRound910ManifestRefsExistInTree spot-checks that every
// TestRef in Round910Manifest names a file that exists and
// contains the named test/fuzz function.
func TestRound910ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round910Manifest {
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

// TestRound910ManifestNoEmptyFields rejects entries that landed
// with a missing field.
func TestRound910ManifestNoEmptyFields(t *testing.T) {
	t.Parallel()
	for i, b := range regression.Round910Manifest {
		if b.PR == "" || b.Finding == 0 || b.Title == "" || b.Symptom == "" || len(b.Tests) == 0 {
			t.Errorf("Round910Manifest[%d] has empty fields: %+v", i, b)
		}
		for j, ref := range b.Tests {
			if ref.PkgRel == "" || ref.TestName == "" || ref.Source == "" {
				t.Errorf("Round910Manifest[%d].Tests[%d] has empty fields: %+v", i, j, ref)
			}
		}
	}
}
