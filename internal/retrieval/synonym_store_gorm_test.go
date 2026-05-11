package retrieval_test

// synonym_store_gorm_test.go — Round-9 Task 4.
//
// SQLite-backed unit tests for the GORM SynonymStore.

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/retrieval"
)

// synonymsSQLiteDDL mirrors migrations/032_synonyms.sql with
// SQLite-compatible types (TEXT in place of JSONB).
const synonymsSQLiteDDL = `CREATE TABLE IF NOT EXISTS retrieval_synonyms (
tenant_id   TEXT NOT NULL,
term        TEXT NOT NULL,
synonyms    TEXT NOT NULL DEFAULT '[]',
updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
PRIMARY KEY (tenant_id, term)
);`

func newSynonymStoreGORMForTest(t *testing.T) *retrieval.SynonymStoreGORM {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.Exec(synonymsSQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := retrieval.NewSynonymStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func TestSynonymStoreGORM_SetAndGet(t *testing.T) {
	t.Parallel()
	s := newSynonymStoreGORMForTest(t)
	ctx := context.Background()
	if err := s.Set(ctx, "t-1", map[string][]string{
		"docs":  {"documents", "papers"},
		"chunk": {"snippet"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := map[string][]string{
		"docs":  {"documents", "papers"},
		"chunk": {"snippet"},
	}
	for k, v := range want {
		gv := got[k]
		sort.Strings(v)
		sort.Strings(gv)
		if !reflect.DeepEqual(v, gv) {
			t.Fatalf("synonyms[%q]: want %v; got %v", k, v, gv)
		}
	}
}

func TestSynonymStoreGORM_SetReplacesAtomically(t *testing.T) {
	t.Parallel()
	s := newSynonymStoreGORMForTest(t)
	ctx := context.Background()
	_ = s.Set(ctx, "t-1", map[string][]string{
		"docs":  {"documents"},
		"chunk": {"snippet"},
	})
	if err := s.Set(ctx, "t-1", map[string][]string{
		"docs": {"papers"},
	}); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	got, _ := s.Get(ctx, "t-1")
	if _, exists := got["chunk"]; exists {
		t.Fatalf("expected chunk to be wiped: %+v", got)
	}
	if !reflect.DeepEqual(got["docs"], []string{"papers"}) {
		t.Fatalf("docs: %v", got["docs"])
	}
}

func TestSynonymStoreGORM_SetEmptyClears(t *testing.T) {
	t.Parallel()
	s := newSynonymStoreGORMForTest(t)
	ctx := context.Background()
	_ = s.Set(ctx, "t-1", map[string][]string{"docs": {"papers"}})
	if err := s.Set(ctx, "t-1", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := s.Get(ctx, "t-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty after clear; got %+v", got)
	}
}

func TestSynonymStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newSynonymStoreGORMForTest(t)
	ctx := context.Background()
	_ = s.Set(ctx, "ta", map[string][]string{"x": {"y"}})
	_ = s.Set(ctx, "tb", map[string][]string{"a": {"b"}})
	gotA, _ := s.Get(ctx, "ta")
	if _, ok := gotA["a"]; ok {
		t.Fatalf("cross-tenant leak: %+v", gotA)
	}
}

func TestSynonymStoreGORM_GetEmpty(t *testing.T) {
	t.Parallel()
	s := newSynonymStoreGORMForTest(t)
	got, err := s.Get(context.Background(), "t-unknown")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map; got %+v", got)
	}
}
