package shard_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// sqliteChunksSchema mirrors storage.Chunk's table layout in
// SQLite-compatible types so the coverage repo can be exercised
// without spinning up Postgres.
const sqliteChunksSchema = `
CREATE TABLE chunks (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    source_id     TEXT NOT NULL,
    document_id   TEXT NOT NULL,
    namespace_id  TEXT,
    block_id      TEXT NOT NULL,
    content_hash  TEXT NOT NULL,
    title         TEXT,
    uri           TEXT,
    connector     TEXT,
    privacy_label TEXT,
    text          TEXT NOT NULL,
    model         TEXT,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_chunks_tenant ON chunks (tenant_id);
`

func newCoverageDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	// SQLite ":memory:" gives each connection its own database, so
	// pool reuse would race the schema/data on writes against fresh
	// connections on reads. One connection keeps every operation in
	// the same in-memory database for the lifetime of the test.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec(sqliteChunksSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

// insertChunkRow inserts a single chunk row directly through gorm's
// raw SQL surface so the repo's read path is exercised against
// data the production storage.PostgresStore would persist.
func insertChunkRow(t *testing.T, db *gorm.DB, tenantID, privacyLabel, id string) {
	t.Helper()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO chunks
		(id, tenant_id, source_id, document_id, block_id, content_hash, privacy_label, text, created_at, updated_at)
		VALUES (?, ?, 'src', 'doc', 'b', 'h', ?, 't', ?, ?)`,
		id, tenantID, privacyLabel, now, now).Error
	if err != nil {
		t.Fatalf("insert chunk %s: %v", id, err)
	}
}

func TestCoverageRepoGORM_CountsTenantChunks(t *testing.T) {
	t.Parallel()
	db := newCoverageDB(t)
	insertChunkRow(t, db, "tenant-a", "internal", "c1")
	insertChunkRow(t, db, "tenant-a", "internal", "c2")
	insertChunkRow(t, db, "tenant-a", "secret", "c3")
	insertChunkRow(t, db, "tenant-b", "internal", "c4")

	repo := shard.NewCoverageRepoGORM(db)

	tests := []struct {
		name   string
		filter shard.ScopeFilter
		want   int
	}{
		{
			name:   "tenant scope counts all rows",
			filter: shard.ScopeFilter{TenantID: "tenant-a"},
			want:   3,
		},
		{
			name:   "privacy mode narrows the count",
			filter: shard.ScopeFilter{TenantID: "tenant-a", PrivacyMode: "internal"},
			want:   2,
		},
		{
			name:   "different tenant is isolated",
			filter: shard.ScopeFilter{TenantID: "tenant-b"},
			want:   1,
		},
		{
			name:   "tenant with no rows returns zero",
			filter: shard.ScopeFilter{TenantID: "tenant-c"},
			want:   0,
		},
		{
			name:   "no matching privacy mode returns zero",
			filter: shard.ScopeFilter{TenantID: "tenant-a", PrivacyMode: "nonexistent"},
			want:   0,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := repo.CorpusChunkCount(context.Background(), tc.filter)
			if err != nil {
				t.Fatalf("CorpusChunkCount: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestCoverageRepoGORM_RequiresTenantScope(t *testing.T) {
	t.Parallel()
	db := newCoverageDB(t)
	repo := shard.NewCoverageRepoGORM(db)
	_, err := repo.CorpusChunkCount(context.Background(), shard.ScopeFilter{})
	if !errors.Is(err, shard.ErrMissingTenantScope) {
		t.Fatalf("err: %v (want ErrMissingTenantScope)", err)
	}
}
