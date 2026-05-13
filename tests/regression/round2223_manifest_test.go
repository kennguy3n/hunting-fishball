package regression_test

// round2223_manifest_test.go — Round-24 Task 8 meta-tests.
//
// Mirrors round1920_manifest_test.go for the Round-22/23
// manifest: every named TestRef must point at a real
// `func TestName(` symbol on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

func TestRound2223ManifestHasFindings(t *testing.T) {
	t.Parallel()
	if len(regression.Round2223Manifest) < 6 {
		t.Fatalf("Round2223Manifest: %d entries, want >= 6", len(regression.Round2223Manifest))
	}
}

func TestRound2223ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round2223Manifest {
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
