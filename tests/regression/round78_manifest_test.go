package regression_test

// round78_manifest_test.go — Round-9 Task 15 meta-tests.
//
// These tests mirror the existing manifest_test.go assertions but
// scope them to Round78Manifest so a future reviewer renaming a
// regression test fails CI immediately, surfacing the rename
// before the manifest drifts from the tree.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

// TestRound78ManifestCoversAllListedBugs confirms the manifest
// enumerates the expected counts: 4 PR-#16 findings + 3 PR-#17
// findings. Adding a new fix must come with both an entry here
// and a matching regression test.
func TestRound78ManifestCoversAllListedBugs(t *testing.T) {
	t.Parallel()
	got := map[string]int{}
	for _, b := range regression.Round78Manifest {
		got[b.PR]++
	}
	if got["16"] != 4 {
		t.Errorf("PR #16: covers %d bugs; want 4", got["16"])
	}
	if got["17"] != 3 {
		t.Errorf("PR #17: covers %d bugs; want 3", got["17"])
	}
}

// TestRound78ManifestRefsExistInTree spot-checks that every
// TestRef in Round78Manifest names a file that exists and contains
// the named test function. Mirrors TestManifestRefsExistInTree
// for the Round-4 manifest.
func TestRound78ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round78Manifest {
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

// TestRound78ManifestNoEmptyFields rejects entries that landed
// without a populated Title/Symptom/Tests. Mirrors the Round-4
// manifest's empty-field guard.
func TestRound78ManifestNoEmptyFields(t *testing.T) {
	t.Parallel()
	for i, b := range regression.Round78Manifest {
		if b.PR == "" {
			t.Errorf("round78[%d]: empty PR", i)
		}
		if b.Title == "" {
			t.Errorf("round78[%d]: empty Title", i)
		}
		if b.Symptom == "" {
			t.Errorf("round78[%d]: empty Symptom", i)
		}
		if len(b.Tests) == 0 {
			t.Errorf("round78[%d]: no tests", i)
		}
		for j, tr := range b.Tests {
			if tr.PkgRel == "" || tr.TestName == "" || tr.Source == "" {
				t.Errorf("round78[%d].Tests[%d]: %+v has empty fields", i, j, tr)
			}
		}
	}
}
