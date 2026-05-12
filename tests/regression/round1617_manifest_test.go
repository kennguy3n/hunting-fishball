package regression_test

// round1617_manifest_test.go — Round-17 Task 12 meta-tests.
//
// Mirrors round1516_manifest_test.go for the Round-17 manifest:
// every named TestRef must point at a real `func TestName(`
// symbol on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

func TestRound1617ManifestHasFindings(t *testing.T) {
	t.Parallel()
	if len(regression.Round1617Manifest) < 3 {
		t.Fatalf("Round1617Manifest: %d entries, want >= 3", len(regression.Round1617Manifest))
	}
}

func TestRound1617ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round1617Manifest {
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
