// embedding_config.go — Round-6 Task 3.
//
// Per-source embedding model configuration. Operators set a non-
// default model on a per-(tenant, source) basis via:
//
//	GET  /v1/admin/sources/:id/embedding
//	PUT  /v1/admin/sources/:id/embedding
//
// The pipeline embed stage reads the row before each document and
// passes the resolved model_name to the embedding gRPC service. A
// missing row falls back to the global default the service was
// deployed with.

package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// SourceEmbeddingConfig is the GORM model for source_embedding_config.
// Stored shape: (tenant_id, source_id) primary key + ML model
// metadata. Mirrors migrations/019_source_embedding_config.sql.
type SourceEmbeddingConfig struct {
	TenantID   string    `gorm:"type:char(26);primaryKey;column:tenant_id" json:"tenant_id"`
	SourceID   string    `gorm:"type:char(26);primaryKey;column:source_id" json:"source_id"`
	ModelName  string    `gorm:"type:varchar(128);not null;column:model_name" json:"model_name"`
	Dimensions int       `gorm:"column:dimensions;not null;default:0" json:"dimensions"`
	CreatedAt  time.Time `gorm:"not null;default:now();column:created_at" json:"created_at"`
	UpdatedAt  time.Time `gorm:"not null;default:now();column:updated_at" json:"updated_at"`
}

// TableName overrides the default GORM pluralisation.
func (SourceEmbeddingConfig) TableName() string { return "source_embedding_config" }

// ErrEmbeddingConfigNotFound is returned by the repository when no
// row matches the (tenant_id, source_id) tuple.
var ErrEmbeddingConfigNotFound = errors.New("admin: embedding config not found")

// EmbeddingConfigRepository is the read/write port for
// source_embedding_config.
type EmbeddingConfigRepository struct {
	db *gorm.DB
}

// NewEmbeddingConfigRepository wires the repository.
func NewEmbeddingConfigRepository(db *gorm.DB) *EmbeddingConfigRepository {
	return &EmbeddingConfigRepository{db: db}
}

// Get returns the per-source embedding config or
// ErrEmbeddingConfigNotFound when no row matches.
func (r *EmbeddingConfigRepository) Get(ctx context.Context, tenantID, sourceID string) (*SourceEmbeddingConfig, error) {
	if tenantID == "" || sourceID == "" {
		return nil, ErrEmbeddingConfigNotFound
	}
	var out SourceEmbeddingConfig
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).
		First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrEmbeddingConfigNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("admin: get embedding config: %w", err)
	}
	return &out, nil
}

// Upsert writes the row, replacing any existing entry. Caller is
// responsible for validating model_name + dimensions.
func (r *EmbeddingConfigRepository) Upsert(ctx context.Context, cfg *SourceEmbeddingConfig) error {
	if cfg == nil || cfg.TenantID == "" || cfg.SourceID == "" || cfg.ModelName == "" {
		return errors.New("admin: invalid embedding config")
	}
	if cfg.Dimensions < 0 {
		return errors.New("admin: dimensions must be non-negative")
	}
	cfg.UpdatedAt = time.Now().UTC()
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = cfg.UpdatedAt
	}
	return r.db.WithContext(ctx).
		Where("tenant_id = ? AND source_id = ?", cfg.TenantID, cfg.SourceID).
		Save(cfg).Error
}

// EmbeddingConfigHandler serves the per-source admin endpoints.
type EmbeddingConfigHandler struct {
	repo  *EmbeddingConfigRepository
	audit AuditWriter
}

// NewEmbeddingConfigHandler validates inputs.
func NewEmbeddingConfigHandler(repo *EmbeddingConfigRepository, aw AuditWriter) (*EmbeddingConfigHandler, error) {
	if repo == nil {
		return nil, errors.New("admin: nil repo")
	}
	if aw == nil {
		aw = noopAudit{}
	}
	return &EmbeddingConfigHandler{repo: repo, audit: aw}, nil
}

// Register mounts the GET / PUT routes.
//
//	GET  /v1/admin/sources/:id/embedding
//	PUT  /v1/admin/sources/:id/embedding
func (h *EmbeddingConfigHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/sources/:id/embedding", h.get)
	rg.PUT("/v1/admin/sources/:id/embedding", h.put)
}

type embeddingPutRequest struct {
	ModelName  string `json:"model_name" binding:"required"`
	Dimensions int    `json:"dimensions"`
}

func (h *EmbeddingConfigHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	cfg, err := h.repo.Get(c.Request.Context(), tenantID, sourceID)
	if errors.Is(err, ErrEmbeddingConfigNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no embedding config for this source"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

func (h *EmbeddingConfigHandler) put(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	sourceID := c.Param("id")
	var req embeddingPutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	cfg := &SourceEmbeddingConfig{
		TenantID:   tenantID,
		SourceID:   sourceID,
		ModelName:  req.ModelName,
		Dimensions: req.Dimensions,
	}
	if err := h.repo.Upsert(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actorID := actorIDFromContext(c)
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, actorID, audit.ActionPolicyEdited, "source", sourceID,
		audit.JSONMap{"embedding_model": req.ModelName, "dimensions": req.Dimensions}, "",
	))
	c.JSON(http.StatusOK, cfg)
}

// EmbeddingConfigResolver is the read-only port the pipeline embed
// stage uses to pick the model_name for a (tenant_id, source_id).
// Returns ("", 0) when no row is configured — the embedding service
// then falls back to its deployment-default model.
type EmbeddingConfigResolver interface {
	ResolveEmbeddingModel(ctx context.Context, tenantID, sourceID string) (modelName string, dimensions int)
}

// Resolve mirrors the EmbeddingConfigResolver contract. Errors are
// swallowed so the embed stage always degrades to the default model.
func (r *EmbeddingConfigRepository) ResolveEmbeddingModel(ctx context.Context, tenantID, sourceID string) (string, int) {
	cfg, err := r.Get(ctx, tenantID, sourceID)
	if err != nil || cfg == nil {
		return "", 0
	}
	return cfg.ModelName, cfg.Dimensions
}
