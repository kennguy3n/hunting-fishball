package regression_test

// round1415_manifest_test.go — Round-15 Task 12 meta-tests.
//
// Mirrors round1213_manifest_test.go for the Round-14/15
// manifest: every named TestRef must point at a real
// `func TestName(` symbol on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

func TestRound1415ManifestHasFindings(t *testing.T) {
	t.Parallel()
	if len(regression.Round1415Manifest) < 3 {
		t.Fatalf("Round1415Manifest: %d entries, want >= 3", len(regression.Round1415Manifest))
	}
}

func TestRound1415ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round1415Manifest {
		for _, ref := range b.Tests {
			path := filepath.Join(root, ref.Source)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("PR %s/%d: source %s missing: %v", b.PR, b.Finding, ref.Source, err)

				continue
			}
			needle := "func " + ref.TestName + "("
			if !strings.Contains(string(data), needle) {
				t.Errorf("PR %s/%d: %s missing function %q", b.PR, b.Finding, ref.Source, needle)
			}
		}
	}
}
