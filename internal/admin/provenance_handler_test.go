package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/storage"
)

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
    text          TEXT NOT NULL DEFAULT '',
    model         TEXT,
    created_at    DATETIME NOT NULL,
    updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_chunks_tenant ON chunks (tenant_id);
`

func newSQLiteChunkRepo(t *testing.T) (*admin.ChunkRepoGORM, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteChunksSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return admin.NewChunkRepoGORM(db), db
}

func seedChunk(t *testing.T, db *gorm.DB, c storage.Chunk) {
	t.Helper()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	if err := db.Create(&c).Error; err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
}

func newProvenanceRouter(t *testing.T, repo admin.ChunkProvenanceLookup, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenant_id", tenantID)
		c.Next()
	})
	h, err := admin.NewChunkProvenanceHandler(repo)
	if err != nil {
		t.Fatalf("NewChunkProvenanceHandler: %v", err)
	}
	rg := &r.RouterGroup
	h.Register(rg)
	return r
}

func TestChunkProvenance_HappyPath(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteChunkRepo(t)
	now := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	seedChunk(t, db, storage.Chunk{
		ID:           "chunk-123",
		TenantID:     "tenant-a",
		SourceID:     "src-1",
		DocumentID:   "doc-1",
		NamespaceID:  "engineering",
		BlockID:      "b1",
		ContentHash:  "abc123",
		Title:        "Design Doc",
		URI:          "https://drive.example/doc-1",
		Connector:    "google-drive",
		PrivacyLabel: "internal",
		Model:        "text-embedding-3-small",
		CreatedAt:    now,
		UpdatedAt:    now.Add(time.Hour),
	})

	r := newProvenanceRouter(t, repo, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/chunks/chunk-123", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var got admin.ChunkProvenance
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ChunkID != "chunk-123" || got.ConnectorType != "google-drive" {
		t.Fatalf("got: %+v", got)
	}
	if got.ContentHash != "abc123" {
		t.Fatalf("ContentHash: %q", got.ContentHash)
	}
	if got.EmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("EmbeddingModel: %q", got.EmbeddingModel)
	}
	if len(got.StorageBackends) < 2 {
		t.Fatalf("StorageBackends: %v", got.StorageBackends)
	}
	if got.NamespaceID != "engineering" {
		t.Fatalf("NamespaceID: %q", got.NamespaceID)
	}
	if !got.IngestedAt.Equal(now) {
		t.Fatalf("IngestedAt: %v want %v", got.IngestedAt, now)
	}
}

func TestChunkProvenance_NotFound(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteChunkRepo(t)
	r := newProvenanceRouter(t, repo, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/chunks/missing", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestChunkProvenance_TenantIsolation(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteChunkRepo(t)
	seedChunk(t, db, storage.Chunk{
		ID: "chunk-x", TenantID: "tenant-b", SourceID: "s", DocumentID: "d", BlockID: "b", ContentHash: "h",
	})
	r := newProvenanceRouter(t, repo, "tenant-a")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/chunks/chunk-x", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant chunk, got %d", w.Code)
	}
}

func TestChunkProvenance_MissingTenantContext(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteChunkRepo(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h, err := admin.NewChunkProvenanceHandler(repo)
	if err != nil {
		t.Fatalf("NewChunkProvenanceHandler: %v", err)
	}
	rg := &r.RouterGroup
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/chunks/x", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestChunkRepoGORM_DirectGet(t *testing.T) {
	t.Parallel()
	repo, db := newSQLiteChunkRepo(t)
	seedChunk(t, db, storage.Chunk{
		ID: "chunk-1", TenantID: "tenant-a", SourceID: "s", DocumentID: "d", BlockID: "b", ContentHash: "h",
	})
	got, err := repo.GetChunk(context.Background(), "tenant-a", "chunk-1")
	if err != nil {
		t.Fatalf("GetChunk: %v", err)
	}
	if got.ID != "chunk-1" {
		t.Fatalf("ID: %q", got.ID)
	}
	// empty tenant or chunk id returns gorm.ErrRecordNotFound
	if _, err := repo.GetChunk(context.Background(), "", "chunk-1"); err == nil {
		t.Fatalf("expected error on empty tenant")
	}
}
