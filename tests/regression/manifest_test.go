package regression_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/tests/regression"
)

// repoRoot walks up from the test's CWD until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("no go.mod found from %s", wd)
	return ""
}

// TestManifestCoversAllPR12Bugs confirms the manifest enumerates
// exactly the six bugs Devin Review caught on PR #12. If a new
// bug is discovered post-merge it must be appended to the
// manifest with a corresponding regression test.
func TestManifestCoversAllPR12Bugs(t *testing.T) {
	t.Parallel()
	count := 0
	for _, b := range regression.Manifest {
		if b.PR == "12" {
			count++
		}
	}
	if count != 6 {
		t.Fatalf("manifest covers %d bugs from PR #12; expected 6", count)
	}
}

// TestManifestRefsExistInTree spot-checks that every TestRef in
// the manifest names a file that exists and contains a function
// declaration with the named test. This guards against
// reviewers renaming a regression test without updating the
// manifest.
func TestManifestRefsExistInTree(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, b := range regression.Manifest {
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

// TestManifestNoEmptyFields rejects entries that landed without
// a populated Title/Symptom/Tests. Empty fields are a sign the
// reviewer added a placeholder bug entry without follow-through.
func TestManifestNoEmptyFields(t *testing.T) {
	t.Parallel()
	for i, b := range regression.Manifest {
		if b.PR == "" {
			t.Errorf("manifest[%d]: empty PR", i)
		}
		if b.Title == "" {
			t.Errorf("manifest[%d]: empty Title", i)
		}
		if b.Symptom == "" {
			t.Errorf("manifest[%d]: empty Symptom", i)
		}
		if len(b.Tests) == 0 {
			t.Errorf("manifest[%d]: no tests", i)
		}
		for j, tr := range b.Tests {
			if tr.PkgRel == "" || tr.TestName == "" || tr.Source == "" {
				t.Errorf("manifest[%d].Tests[%d]: %+v has empty fields", i, j, tr)
			}
		}
	}
}
