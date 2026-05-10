// Package rollback verifies that every forward migration has a
// corresponding rollback file.
//
// Round-4 Task 20: a deploy that needs to roll back a schema
// change today has nothing to apply; this package backstops the
// catalog and pins it with a test so a forgotten rollback fails
// loudly.
//
// We deliberately do NOT execute the rollback files against
// SQLite during this test — most of them target Postgres-only
// syntax (TIMESTAMPTZ, JSONB, ALTER COLUMN TYPE) that SQLite
// does not understand. The e2e suite covers the actual rollback
// behaviour against a real Postgres. Here we statically validate
// the file presence + minimal structural sanity (must contain
// DROP/ALTER, must not be empty).
package rollback_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func rollbackDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	return filepath.Dir(rollbackDir(t))
}

func listSQL(t *testing.T, dir, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestRollbacksExistForEveryForwardMigration locks the contract
// that every NNN_*.sql in migrations/ has a corresponding
// NNN_*.down.sql in migrations/rollback/. The check is by
// numeric prefix so a renamed forward migration still matches.
func TestRollbacksExistForEveryForwardMigration(t *testing.T) {
	t.Parallel()
	fwd := listSQL(t, migrationsDir(t), ".sql")
	rev := listSQL(t, rollbackDir(t), ".down.sql")
	revPrefixes := map[string]string{}
	for _, n := range rev {
		idx := strings.IndexByte(n, '_')
		if idx > 0 {
			revPrefixes[n[:idx]] = n
		}
	}
	for _, n := range fwd {
		idx := strings.IndexByte(n, '_')
		if idx <= 0 {
			continue
		}
		prefix := n[:idx]
		if _, ok := revPrefixes[prefix]; !ok {
			t.Errorf("forward migration %s has no rollback (expected migrations/rollback/%s_*.down.sql)", n, prefix)
		}
	}
}

// TestRollbacksAreNonEmpty rejects rollback files that contain
// only comments. A reviewer who ships a placeholder rollback by
// accident gets caught here.
func TestRollbacksAreNonEmpty(t *testing.T) {
	t.Parallel()
	rev := listSQL(t, rollbackDir(t), ".down.sql")
	if len(rev) == 0 {
		t.Fatal("no rollback files found")
	}
	for _, n := range rev {
		body, err := os.ReadFile(filepath.Join(rollbackDir(t), n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		hasStmt := false
		for _, line := range strings.Split(string(body), "\n") {
			trim := strings.TrimSpace(line)
			if trim == "" || strings.HasPrefix(trim, "--") {
				continue
			}
			upper := strings.ToUpper(trim)
			if strings.HasPrefix(upper, "DROP ") || strings.HasPrefix(upper, "ALTER ") {
				hasStmt = true
				break
			}
		}
		if !hasStmt {
			t.Errorf("%s contains no DROP/ALTER statements; placeholder rollback?", n)
		}
	}
}
