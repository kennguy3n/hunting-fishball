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

const sqliteShardSchema = `
CREATE TABLE shards (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    user_id       TEXT,
    channel_id    TEXT,
    privacy_mode  TEXT NOT NULL,
    shard_version INTEGER NOT NULL DEFAULT 1,
    chunks_count  INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_shards_tenant ON shards (tenant_id, user_id, channel_id);

CREATE TABLE shard_chunks (
    shard_id  TEXT NOT NULL,
    chunk_id  TEXT NOT NULL,
    tenant_id TEXT NOT NULL,
    PRIMARY KEY (shard_id, chunk_id)
);
CREATE INDEX idx_shard_chunks ON shard_chunks (tenant_id, shard_id);

CREATE TABLE tenant_lifecycle (
    tenant_id    TEXT PRIMARY KEY,
    state        TEXT NOT NULL,
    requested_by TEXT,
    requested_at DATETIME NOT NULL,
    deleted_at   DATETIME
);
`

func newTestRepo(t *testing.T) *shard.Repository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm open: %v", err)
	}
	if err := db.Exec(sqliteShardSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return shard.NewRepository(db)
}

func TestRepository_CreateAndList(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	m := &shard.ShardManifest{
		TenantID:     "tenant-a",
		UserID:       "user-1",
		ChannelID:    "channel-1",
		PrivacyMode:  "internal",
		ShardVersion: 1,
		Status:       shard.ShardStatusReady,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.ID == "" {
		t.Fatal("expected ID minted")
	}

	rows, err := repo.List(ctx, shard.ScopeFilter{TenantID: "tenant-a"}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].TenantID != "tenant-a" {
		t.Fatalf("tenant mismatch: %q", rows[0].TenantID)
	}
}

func TestRepository_RefusesMissingTenantScope(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, &shard.ShardManifest{}); !errors.Is(err, shard.ErrMissingTenantScope) {
		t.Fatalf("expected missing-tenant error, got %v", err)
	}
	if _, err := repo.List(ctx, shard.ScopeFilter{}, 0); !errors.Is(err, shard.ErrMissingTenantScope) {
		t.Fatalf("expected missing-tenant error, got %v", err)
	}
	if _, err := repo.LatestVersion(ctx, shard.ScopeFilter{}); !errors.Is(err, shard.ErrMissingTenantScope) {
		t.Fatalf("expected missing-tenant error, got %v", err)
	}
}

func TestRepository_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.Create(ctx, &shard.ShardManifest{
		TenantID:     "tenant-a",
		PrivacyMode:  "internal",
		ShardVersion: 1,
		Status:       shard.ShardStatusReady,
	}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if err := repo.Create(ctx, &shard.ShardManifest{
		TenantID:     "tenant-b",
		PrivacyMode:  "internal",
		ShardVersion: 1,
		Status:       shard.ShardStatusReady,
	}); err != nil {
		t.Fatalf("create B: %v", err)
	}

	rows, err := repo.List(ctx, shard.ScopeFilter{TenantID: "tenant-a"}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].TenantID != "tenant-a" {
		t.Fatalf("cross-tenant leak: %+v", rows)
	}
}

func TestRepository_LatestVersion(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	for v := int64(1); v <= 3; v++ {
		if err := repo.Create(ctx, &shard.ShardManifest{
			TenantID:     "tenant-a",
			UserID:       "user-1",
			ChannelID:    "channel-1",
			PrivacyMode:  "internal",
			ShardVersion: v,
			Status:       shard.ShardStatusReady,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create v%d: %v", v, err)
		}
	}

	v, err := repo.LatestVersion(ctx, shard.ScopeFilter{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	})
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if v != 3 {
		t.Fatalf("want 3, got %d", v)
	}
}

func TestRepository_ChunkIDsRoundTrip(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	m := &shard.ShardManifest{
		TenantID:     "tenant-a",
		PrivacyMode:  "internal",
		ShardVersion: 1,
		Status:       shard.ShardStatusReady,
	}
	if err := repo.Create(ctx, m); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := []string{"chunk-1", "chunk-2", "chunk-3"}
	if err := repo.SetChunkIDs(ctx, "tenant-a", m.ID, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := repo.ChunkIDs(ctx, "tenant-a", m.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: %v vs %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d: %q vs %q", i, got[i], want[i])
		}
	}

	// Replacing the set drops the old rows.
	next := []string{"chunk-9"}
	if err := repo.SetChunkIDs(ctx, "tenant-a", m.ID, next); err != nil {
		t.Fatalf("set2: %v", err)
	}
	got, err = repo.ChunkIDs(ctx, "tenant-a", m.ID)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if len(got) != 1 || got[0] != "chunk-9" {
		t.Fatalf("replace failed: %v", got)
	}
}

func TestRepository_MarkSuperseded(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	for v := int64(1); v <= 3; v++ {
		if err := repo.Create(ctx, &shard.ShardManifest{
			TenantID:     "tenant-a",
			UserID:       "user-1",
			ChannelID:    "channel-1",
			PrivacyMode:  "internal",
			ShardVersion: v,
			Status:       shard.ShardStatusReady,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	scope := shard.ScopeFilter{
		TenantID:    "tenant-a",
		UserID:      "user-1",
		ChannelID:   "channel-1",
		PrivacyMode: "internal",
	}
	if err := repo.MarkSuperseded(ctx, scope, 3); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	rows, err := repo.List(ctx, shard.ScopeFilter{
		TenantID: "tenant-a",
		Status:   shard.ShardStatusSuperseded,
	}, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 superseded, got %d", len(rows))
	}
}

func TestRepository_LifecycleStateRoundTrip(t *testing.T) {
	t.Parallel()
	repo := newTestRepo(t)
	ctx := context.Background()

	state, err := repo.LifecycleState(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("initial: %v", err)
	}
	if state != "" {
		t.Fatalf("want empty, got %q", state)
	}

	if err := repo.MarkLifecycle(ctx, "tenant-a", shard.LifecyclePendingDeletion, "admin-1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	state, err = repo.LifecycleState(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("after mark: %v", err)
	}
	if state != shard.LifecyclePendingDeletion {
		t.Fatalf("want pending, got %q", state)
	}

	if err := repo.MarkLifecycle(ctx, "tenant-a", shard.LifecycleDeleted, "admin-1"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	state, err = repo.LifecycleState(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("after deleted: %v", err)
	}
	if state != shard.LifecycleDeleted {
		t.Fatalf("want deleted, got %q", state)
	}
}
