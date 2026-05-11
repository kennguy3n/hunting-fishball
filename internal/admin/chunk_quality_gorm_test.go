package admin_test

// chunk_quality_gorm_test.go — Round-9 Task 5.

import (
	"context"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// chunkQualitySQLiteDDL mirrors migrations/028_chunk_quality.sql
// with SQLite-compatible types.
const chunkQualitySQLiteDDL = `CREATE TABLE IF NOT EXISTS chunk_quality (
tenant_id     TEXT NOT NULL,
source_id     TEXT NOT NULL,
document_id   TEXT NOT NULL,
chunk_id      TEXT NOT NULL,
quality_score REAL NOT NULL DEFAULT 0,
length_score  REAL NOT NULL DEFAULT 0,
lang_score    REAL NOT NULL DEFAULT 0,
embed_score   REAL NOT NULL DEFAULT 0,
duplicate     BOOLEAN NOT NULL DEFAULT 0,
updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
PRIMARY KEY (tenant_id, chunk_id)
);`

func newChunkQualityStoreGORMForTest(t *testing.T) *admin.ChunkQualityStoreGORM {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.Exec(chunkQualitySQLiteDDL).Error; err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := admin.NewChunkQualityStoreGORM(db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return s
}

func TestChunkQualityStoreGORM_InsertAndList(t *testing.T) {
	t.Parallel()
	s := newChunkQualityStoreGORMForTest(t)
	ctx := context.Background()
	rows := []admin.ChunkQualityRow{
		{TenantID: "t-1", SourceID: "s-1", DocumentID: "d-1", ChunkID: "c-1", QualityScore: 0.9},
		{TenantID: "t-1", SourceID: "s-1", DocumentID: "d-1", ChunkID: "c-2", QualityScore: 0.3, Duplicate: true},
		{TenantID: "t-1", SourceID: "s-2", DocumentID: "d-2", ChunkID: "c-3", QualityScore: 0.7},
	}
	for _, r := range rows {
		if err := s.Insert(ctx, r); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	got, err := s.ListByTenant(ctx, "t-1", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows; got %d", len(got))
	}
}

func TestChunkQualityStoreGORM_InsertIsUpsert(t *testing.T) {
	t.Parallel()
	s := newChunkQualityStoreGORMForTest(t)
	ctx := context.Background()
	if err := s.Insert(ctx, admin.ChunkQualityRow{
		TenantID: "t-1", SourceID: "s-1", DocumentID: "d-1", ChunkID: "c-1", QualityScore: 0.4,
	}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	// Re-scoring the same chunk overwrites the row.
	if err := s.Insert(ctx, admin.ChunkQualityRow{
		TenantID: "t-1", SourceID: "s-1", DocumentID: "d-1", ChunkID: "c-1", QualityScore: 0.9, Duplicate: true,
	}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	got, _ := s.ListByTenant(ctx, "t-1", 100)
	if len(got) != 1 {
		t.Fatalf("expected 1 row after upsert; got %d", len(got))
	}
	if got[0].QualityScore != 0.9 || !got[0].Duplicate {
		t.Fatalf("upsert didn't overwrite: %+v", got[0])
	}
}

func TestChunkQualityStoreGORM_TenantIsolation(t *testing.T) {
	t.Parallel()
	s := newChunkQualityStoreGORMForTest(t)
	ctx := context.Background()
	_ = s.Insert(ctx, admin.ChunkQualityRow{TenantID: "ta", SourceID: "s", DocumentID: "d", ChunkID: "c-a"})
	_ = s.Insert(ctx, admin.ChunkQualityRow{TenantID: "tb", SourceID: "s", DocumentID: "d", ChunkID: "c-b"})
	got, _ := s.ListByTenant(ctx, "ta", 100)
	if len(got) != 1 || got[0].ChunkID != "c-a" {
		t.Fatalf("cross-tenant leak: %+v", got)
	}
}

func TestChunkQualityStoreGORM_InsertRejectsEmpty(t *testing.T) {
	t.Parallel()
	s := newChunkQualityStoreGORMForTest(t)
	if err := s.Insert(context.Background(), admin.ChunkQualityRow{TenantID: "", ChunkID: "c"}); err == nil {
		t.Fatalf("expected error for missing tenant")
	}
}
