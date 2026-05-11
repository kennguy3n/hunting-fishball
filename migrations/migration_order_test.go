// Package migrations holds the SQL forward migrations and a
// structural test that guards the prefix discipline.
//
// Round-11 Task 14: the prior rollback test (migrations/rollback/
// rollback_test.go) only checked file presence. This test now
// additionally asserts:
//
//  1. No duplicate numeric prefixes. Two engineers merging
//     concurrent PRs both naming their migration NNN_*.sql
//     silently overwrite each other in the migration runner —
//     the test catches the duplicate at CI time.
//  2. Prefixes are strictly monotonically increasing starting
//     from 001. A gap (e.g. 029 -> 031) usually means a deleted
//     migration that the rollback file was orphaned from; we
//     surface that gap so the engineer can re-number.
//  3. Every forward migration has a matching rollback file. This
//     is the assertion the original rollback_test.go owned; we
//     keep it here so both invariants land in one report.
package migrations_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func forwardMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func rollbackMigrations(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(".", "rollback"))
	if err != nil {
		t.Fatalf("read rollback dir: %v", err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".down.sql") {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// extractPrefix returns the leading "NNN" integer prefix of a
// migration file name (forward: "NNN_*.sql"; rollback:
// "NNN_*.down.sql"). Returns -1 when the name doesn't start with
// digits.
func extractPrefix(name string) int {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return -1
	}
	n, err := strconv.Atoi(name[:idx])
	if err != nil {
		return -1
	}
	return n
}

// TestMigrationPrefixes_NoDuplicates — Round-11 Task 14 (a).
func TestMigrationPrefixes_NoDuplicates(t *testing.T) {
	t.Parallel()
	seen := map[int]string{}
	for _, name := range forwardMigrations(t) {
		n := extractPrefix(name)
		if n < 0 {
			t.Errorf("migration without numeric prefix: %s", name)
			continue
		}
		if existing, dup := seen[n]; dup {
			t.Errorf("duplicate migration prefix %03d: %s vs %s", n, existing, name)
			continue
		}
		seen[n] = name
	}
}

// TestMigrationPrefixes_Monotonic — Round-11 Task 14 (b).
//
// Asserts the prefix sequence is strictly increasing and starts
// at 1. A gap fails the test so a deleted migration doesn't go
// unnoticed; the next migration must reuse the slot or the test
// owner must explicitly skip the gap by extending this list.
func TestMigrationPrefixes_Monotonic(t *testing.T) {
	t.Parallel()
	files := forwardMigrations(t)
	if len(files) == 0 {
		t.Fatal("no forward migrations found")
	}
	prefixes := make([]int, 0, len(files))
	for _, name := range files {
		n := extractPrefix(name)
		if n < 0 {
			continue
		}
		prefixes = append(prefixes, n)
	}
	sort.Ints(prefixes)
	for i, n := range prefixes {
		want := i + 1
		if n != want {
			t.Errorf("non-monotonic prefix at index %d: got %03d, expected %03d (gap or duplicate)", i, n, want)
		}
	}
}

// TestMigrationPrefixes_RollbackParity — Round-11 Task 14 (c).
//
// Every forward migration must have a matching rollback file with
// the same prefix. We don't require name-suffix parity (only
// prefix + extension) so renamed migrations don't churn the
// rollback files.
func TestMigrationPrefixes_RollbackParity(t *testing.T) {
	t.Parallel()
	fwd := forwardMigrations(t)
	back := rollbackMigrations(t)
	backByPrefix := map[int]string{}
	for _, name := range back {
		n := extractPrefix(name)
		if n < 0 {
			continue
		}
		backByPrefix[n] = name
	}
	for _, name := range fwd {
		n := extractPrefix(name)
		if n < 0 {
			continue
		}
		if _, ok := backByPrefix[n]; !ok {
			t.Errorf("forward migration %s has no rollback file (expected %s)", name, fmt.Sprintf("rollback/%03d_*.down.sql", n))
		}
	}
}
