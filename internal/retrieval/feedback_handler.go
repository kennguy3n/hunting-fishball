// feedback_handler.go — Round-5 Task 16.
//
// Retrieval feedback is the bridge between live traffic and the
// eval harness. Every relevance signal an admin or end-user
// submits via POST /v1/retrieve/feedback lands in
// feedback_events; the eval runner consumes those rows as
// ground-truth alongside the curated golden corpus from Task 1
// so the precision/MRR regression gate adapts to real-world
// retrieval distributions.
//
// The endpoint is intentionally narrow: a single per-(query,
// chunk) thumbs-up/thumbs-down with an optional free-text note.
// We do not interpret the relevance signal here — that is the
// eval runner's job — we just persist it with strict tenant
// isolation so a malicious caller cannot taint another tenant's
// ground-truth.
package retrieval

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// FeedbackEvent is one persisted relevance signal. The shape
// mirrors migrations/018_feedback_events.sql exactly.
type FeedbackEvent struct {
	ID        string    `gorm:"type:char(26);primaryKey;column:id" json:"id"`
	TenantID  string    `gorm:"type:char(26);not null;column:tenant_id" json:"tenant_id"`
	QueryID   string    `gorm:"type:varchar(64);not null;column:query_id" json:"query_id"`
	ChunkID   string    `gorm:"type:varchar(128);not null;column:chunk_id" json:"chunk_id"`
	UserID    string    `gorm:"type:varchar(64);not null;default:'';column:user_id" json:"user_id"`
	Relevant  bool      `gorm:"not null;column:relevant" json:"relevant"`
	Note      string    `gorm:"type:text;not null;default:'';column:note" json:"note"`
	CreatedAt time.Time `gorm:"type:timestamptz;not null;default:now();column:created_at" json:"created_at"`
}

// TableName overrides GORM's default plural-snake naming so the
// row maps onto feedback_events exactly.
func (FeedbackEvent) TableName() string { return "feedback_events" }

// FeedbackRepository is the GORM-backed accessor for
// feedback_events. The same handle drives Create from the
// /v1/retrieve/feedback handler and ListByTenant from the eval
// runner so feedback is read in the same transaction the
// runner uses to materialise its ground-truth view.
type FeedbackRepository struct {
	db *gorm.DB
}

// NewFeedbackRepository wraps a *gorm.DB.
func NewFeedbackRepository(db *gorm.DB) *FeedbackRepository {
	return &FeedbackRepository{db: db}
}

// DB exposes the underlying *gorm.DB so callers can run
// transactions. Mirrors PolicyVersionRepository.DB().
func (r *FeedbackRepository) DB() *gorm.DB { return r.db }

// Create inserts a single feedback row. ID and CreatedAt are
// minted server-side so callers can't forge ULIDs that collide
// with another tenant's row.
func (r *FeedbackRepository) Create(ctx context.Context, e *FeedbackEvent) error {
	if e == nil {
		return errors.New("feedback: nil event")
	}
	if e.TenantID == "" {
		return errors.New("feedback: missing tenant_id")
	}
	if e.QueryID == "" {
		return errors.New("feedback: missing query_id")
	}
	if e.ChunkID == "" {
		return errors.New("feedback: missing chunk_id")
	}
	if e.ID == "" {
		e.ID = ulid.Make().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Create(e).Error
}

// FeedbackFilter narrows ListByTenant.
type FeedbackFilter struct {
	TenantID string
	QueryID  string // optional
	ChunkID  string // optional
	Limit    int    // 0 → 200; capped at 1000 (eval runner pulls in bulk)
	Cursor   string // ULID — rows with id < Cursor are returned
}

// ListByTenant returns feedback rows for a tenant in
// descending-id order so the eval runner sees the newest signal
// first. The eval runner is the only caller that needs the
// 1000-row cap; the public API (admin list, future) will use the
// 200-row default mirroring Task 3.
func (r *FeedbackRepository) ListByTenant(ctx context.Context, f FeedbackFilter) ([]FeedbackEvent, error) {
	if f.TenantID == "" {
		return nil, errors.New("feedback: missing tenant_id")
	}
	if f.Limit <= 0 {
		f.Limit = 200
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	q := r.db.WithContext(ctx).
		Where("tenant_id = ?", f.TenantID).
		Order("id DESC").
		Limit(f.Limit)
	if f.QueryID != "" {
		q = q.Where("query_id = ?", f.QueryID)
	}
	if f.ChunkID != "" {
		q = q.Where("chunk_id = ?", f.ChunkID)
	}
	if f.Cursor != "" {
		q = q.Where("id < ?", f.Cursor)
	}
	var out []FeedbackEvent
	if err := q.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// FeedbackHandlerConfig wires the HTTP handler.
type FeedbackHandlerConfig struct {
	Repo  *FeedbackRepository
	Audit AuditWriter
}

// AuditWriter mirrors the narrow contract used by the
// internal/admin handlers. Defined locally to avoid an admin →
// retrieval import cycle.
type AuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// FeedbackHandler exposes the retrieval feedback endpoints.
type FeedbackHandler struct {
	cfg FeedbackHandlerConfig
}

// NewFeedbackHandler validates cfg and returns a handler.
func NewFeedbackHandler(cfg FeedbackHandlerConfig) (*FeedbackHandler, error) {
	if cfg.Repo == nil {
		return nil, errors.New("feedback: missing Repo")
	}
	if cfg.Audit == nil {
		return nil, errors.New("feedback: missing Audit")
	}
	return &FeedbackHandler{cfg: cfg}, nil
}

// RegisterFeedback mounts the routes onto rg. The expected
// mount path is /v1 so the resulting endpoint is
// /v1/retrieve/feedback.
func (h *FeedbackHandler) RegisterFeedback(rg *gin.RouterGroup) {
	rg.POST("/retrieve/feedback", h.submit)
}

// FeedbackRequest is the JSON shape of POST /v1/retrieve/feedback.
type FeedbackRequest struct {
	QueryID  string `json:"query_id" binding:"required"`
	ChunkID  string `json:"chunk_id" binding:"required"`
	Relevant *bool  `json:"relevant" binding:"required"`
	Note     string `json:"note,omitempty"`
}

// FeedbackResponse echoes the persisted row so admins can spot
// race conditions (two admins thumbs-down'ing the same chunk).
type FeedbackResponse struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *FeedbackHandler) submit(c *gin.Context) {
	tenantID, ok := tenantIDFromContextRetrieval(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var req FeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.QueryID) == "" || strings.TrimSpace(req.ChunkID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query_id and chunk_id required"})
		return
	}
	if req.Relevant == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "relevant must be set"})
		return
	}
	userID := actorIDFromContextRetrieval(c)
	e := &FeedbackEvent{
		TenantID: tenantID,
		QueryID:  strings.TrimSpace(req.QueryID),
		ChunkID:  strings.TrimSpace(req.ChunkID),
		UserID:   userID,
		Relevant: *req.Relevant,
		Note:     req.Note,
	}
	if err := h.cfg.Repo.Create(c.Request.Context(), e); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.cfg.Audit.Create(c.Request.Context(), audit.NewAuditLog(
		tenantID, userID, audit.ActionRetrievalFeedback, "chunk", e.ChunkID,
		audit.JSONMap{
			"query_id": e.QueryID,
			"relevant": e.Relevant,
		},
		"",
	))
	c.JSON(http.StatusOK, FeedbackResponse{ID: e.ID, CreatedAt: e.CreatedAt})
}

// tenantIDFromContextRetrieval mirrors the admin-handler lookup.
func tenantIDFromContextRetrieval(c *gin.Context) (string, bool) {
	v, ok := c.Get(audit.TenantContextKey)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func actorIDFromContextRetrieval(c *gin.Context) string {
	v, ok := c.Get(audit.ActorContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
