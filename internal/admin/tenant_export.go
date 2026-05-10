package admin

// tenant_export.go — Round-6 Task 14.
//
// Full tenant data export. Operators submit
//
//   POST /v1/admin/tenants/:tenant_id/export
//
// which returns a job ID. The actual collection (sources,
// policies, audit logs, chunks metadata) happens asynchronously.
// Clients poll
//
//   GET /v1/admin/tenants/:tenant_id/export/:job_id
//
// for status + download URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// TenantExportStatus enumerates the lifecycle of an export job.
type TenantExportStatus string

const (
	// TenantExportStatusPending — accepted but not yet started.
	TenantExportStatusPending TenantExportStatus = "pending"
	// TenantExportStatusRunning — currently collecting.
	TenantExportStatusRunning TenantExportStatus = "running"
	// TenantExportStatusCompleted — payload ready for download.
	TenantExportStatusCompleted TenantExportStatus = "completed"
	// TenantExportStatusFailed — collection failed; see Error.
	TenantExportStatusFailed TenantExportStatus = "failed"
)

// TenantExportJob is the persisted shape.
type TenantExportJob struct {
	ID           string             `json:"id"`
	TenantID     string             `json:"tenant_id"`
	Status       TenantExportStatus `json:"status"`
	Error        string             `json:"error,omitempty"`
	DownloadURL  string             `json:"download_url,omitempty"`
	PayloadBytes int                `json:"payload_bytes,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	UpdatedAt    time.Time          `json:"updated_at"`
}

// TenantExportPayload is the structured archive returned to the
// caller. Each section is an opaque JSON document so the export
// schema can grow without churning the migration history.
type TenantExportPayload struct {
	TenantID       string            `json:"tenant_id"`
	GeneratedAt    time.Time         `json:"generated_at"`
	Sources        []json.RawMessage `json:"sources"`
	Policies       []json.RawMessage `json:"policies"`
	AuditLogs      []json.RawMessage `json:"audit_logs"`
	ChunksMetadata []json.RawMessage `json:"chunks_metadata"`
}

// TenantExportCollector collects the per-section data for a
// tenant. The production wiring composes Postgres + retrieval-side
// queries; tests pass a stub.
type TenantExportCollector interface {
	Collect(ctx context.Context, tenantID string) (*TenantExportPayload, error)
}

// TenantExportPublisher writes the collected payload somewhere
// addressable (S3, Postgres LO, in-memory blob store) and returns
// a download URL.
type TenantExportPublisher interface {
	Publish(ctx context.Context, jobID string, payload []byte) (downloadURL string, err error)
}

// TenantExportJobStore persists export job rows.
type TenantExportJobStore interface {
	Create(ctx context.Context, job *TenantExportJob) error
	Update(ctx context.Context, job *TenantExportJob) error
	Get(ctx context.Context, tenantID, jobID string) (*TenantExportJob, error)
}

// InMemoryTenantExportJobStore is a goroutine-safe map-backed
// store for tests + local dev.
type InMemoryTenantExportJobStore struct {
	mu   sync.RWMutex
	rows map[string]*TenantExportJob
	seq  int64
}

// NewInMemoryTenantExportJobStore constructs the in-memory store.
func NewInMemoryTenantExportJobStore() *InMemoryTenantExportJobStore {
	return &InMemoryTenantExportJobStore{rows: map[string]*TenantExportJob{}}
}

// Create implements TenantExportJobStore.
func (s *InMemoryTenantExportJobStore) Create(_ context.Context, job *TenantExportJob) error {
	if job == nil {
		return errors.New("nil job")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if job.ID == "" {
		s.seq++
		job.ID = fmt.Sprintf("export-%d", s.seq)
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	cp := *job
	s.rows[job.ID] = &cp
	return nil
}

// Update implements TenantExportJobStore.
func (s *InMemoryTenantExportJobStore) Update(_ context.Context, job *TenantExportJob) error {
	if job == nil {
		return errors.New("nil job")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job.UpdatedAt = time.Now().UTC()
	cp := *job
	s.rows[job.ID] = &cp
	return nil
}

// Get implements TenantExportJobStore.
func (s *InMemoryTenantExportJobStore) Get(_ context.Context, tenantID, jobID string) (*TenantExportJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.rows[jobID]
	if !ok || j.TenantID != tenantID {
		return nil, errors.New("export job: not found")
	}
	cp := *j
	return &cp, nil
}

// TenantExportConfig wires the handler.
type TenantExportConfig struct {
	JobStore  TenantExportJobStore
	Collector TenantExportCollector
	Publisher TenantExportPublisher
	Audit     AuditWriter
}

// TenantExportHandler exposes the full-export admin surface.
type TenantExportHandler struct {
	cfg TenantExportConfig
}

// NewTenantExportHandler validates cfg and returns a handler.
func NewTenantExportHandler(cfg TenantExportConfig) (*TenantExportHandler, error) {
	if cfg.JobStore == nil {
		return nil, errors.New("tenant export: nil JobStore")
	}
	if cfg.Collector == nil {
		return nil, errors.New("tenant export: nil Collector")
	}
	if cfg.Publisher == nil {
		return nil, errors.New("tenant export: nil Publisher")
	}
	return &TenantExportHandler{cfg: cfg}, nil
}

// Register attaches routes.
func (h *TenantExportHandler) Register(g *gin.RouterGroup) {
	g.POST("/v1/admin/tenants/:tenant_id/export", h.create)
	g.GET("/v1/admin/tenants/:tenant_id/export/:job_id", h.get)
}

func (h *TenantExportHandler) create(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	requestTenant, _ := c.Get(audit.TenantContextKey)
	if rt, _ := requestTenant.(string); rt == "" || rt != tenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant scope mismatch"})
		return
	}
	job := &TenantExportJob{
		TenantID: tenantID, Status: TenantExportStatusPending,
	}
	if err := h.cfg.JobStore.Create(c.Request.Context(), job); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	go func() { _ = h.runJob(context.Background(), job.ID, tenantID) }()
	c.JSON(http.StatusAccepted, job)
}

// RunJobSync executes the export inline. Useful for tests; the
// HTTP handler always launches asynchronously.
func (h *TenantExportHandler) RunJobSync(ctx context.Context, jobID, tenantID string) error {
	return h.runJob(ctx, jobID, tenantID)
}

func (h *TenantExportHandler) runJob(ctx context.Context, jobID, tenantID string) error {
	job, err := h.cfg.JobStore.Get(ctx, tenantID, jobID)
	if err != nil {
		return err
	}
	job.Status = TenantExportStatusRunning
	_ = h.cfg.JobStore.Update(ctx, job)

	payload, err := h.cfg.Collector.Collect(ctx, tenantID)
	if err != nil {
		job.Status = TenantExportStatusFailed
		job.Error = err.Error()
		_ = h.cfg.JobStore.Update(ctx, job)
		return err
	}
	if payload == nil {
		payload = &TenantExportPayload{TenantID: tenantID, GeneratedAt: time.Now().UTC()}
	}
	if payload.GeneratedAt.IsZero() {
		payload.GeneratedAt = time.Now().UTC()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		job.Status = TenantExportStatusFailed
		job.Error = err.Error()
		_ = h.cfg.JobStore.Update(ctx, job)
		return err
	}
	url, err := h.cfg.Publisher.Publish(ctx, jobID, body)
	if err != nil {
		job.Status = TenantExportStatusFailed
		job.Error = err.Error()
		_ = h.cfg.JobStore.Update(ctx, job)
		return err
	}
	job.Status = TenantExportStatusCompleted
	job.DownloadURL = url
	job.PayloadBytes = len(body)
	_ = h.cfg.JobStore.Update(ctx, job)
	return nil
}

func (h *TenantExportHandler) get(c *gin.Context) {
	tenantID := c.Param("tenant_id")
	requestTenant, _ := c.Get(audit.TenantContextKey)
	if rt, _ := requestTenant.(string); rt == "" || rt != tenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant scope mismatch"})
		return
	}
	job, err := h.cfg.JobStore.Get(c.Request.Context(), tenantID, c.Param("job_id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, job)
}
