package regression_test

// round1718_manifest_test.go — Round-18 Task 16 meta-tests.
//
// Mirrors round1617_manifest_test.go for the Round-18 manifest:
// every named TestRef must point at a real `func TestName(`
// symbol on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

func TestRound1718ManifestHasFindings(t *testing.T) {
	t.Parallel()
	if len(regression.Round1718Manifest) < 6 {
		t.Fatalf("Round1718Manifest: %d entries, want >= 6", len(regression.Round1718Manifest))
	}
}

func TestRound1718ManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Round1718Manifest {
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
