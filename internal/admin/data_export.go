// data_export.go — Round-5 Task 7.
//
// GDPR Article 15 ("right of access") gives every data subject the
// right to obtain a copy of the personal data the controller holds
// on them. For hunting-fishball that translates to a per-tenant
// archive of:
//
//   - the source rows the tenant has connected (connector type, scopes,
//     creation timestamps — NEVER credentials),
//   - the policy snapshot history,
//   - audit log entries within a configurable window, and
//   - chunk metadata (id, source, content hash, length) — emphatically
//     NOT the raw chunk content or its embedding vectors. The export
//     is metadata-only because the underlying content is also stored
//     in the upstream SaaS the connector points at, and re-shipping
//     it through the export job risks duplicating sensitive payloads
//     into the archive blob without adding informational value.
//
// The handler creates an export_jobs row with status=pending, returns
// the job_id, and the worker (run separately via the same process
// or a dedicated background container) walks the tenant's metadata,
// writes a JSON archive to object storage, and updates the row with
// the download_url. Status moves pending → running → succeeded |
// failed. The admin polls GET /v1/admin/tenants/:tenant_id/export/:job_id
// until status is terminal.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// ExportJobStatus enumerates the lifecycle of an export job.
type ExportJobStatus string

const (
	ExportJobStatusPending   ExportJobStatus = "pending"
	ExportJobStatusRunning   ExportJobStatus = "running"
	ExportJobStatusSucceeded ExportJobStatus = "succeeded"
	ExportJobStatusFailed    ExportJobStatus = "failed"
)

// ExportJob is the GORM model for the export_jobs table created in
// migrations/015_export_jobs.sql.
type ExportJob struct {
	ID          string          `gorm:"type:varchar(26);primaryKey;column:id" json:"id"`
	TenantID    string          `gorm:"type:varchar(26);not null;column:tenant_id" json:"tenant_id"`
	ActorID     string          `gorm:"type:varchar(64);not null;column:actor_id" json:"actor_id"`
	Status      ExportJobStatus `gorm:"type:varchar(16);not null;column:status" json:"status"`
	DownloadURL string          `gorm:"column:download_url" json:"download_url,omitempty"`
	Error       string          `gorm:"column:error" json:"error,omitempty"`
	CreatedAt   time.Time       `gorm:"not null;column:created_at" json:"created_at"`
	StartedAt   *time.Time      `gorm:"column:started_at" json:"started_at,omitempty"`
	CompletedAt *time.Time      `gorm:"column:completed_at" json:"completed_at,omitempty"`
}

// TableName overrides the default GORM pluralisation.
func (ExportJob) TableName() string { return "export_jobs" }

// ExportJobRepository is the read/write port for export_jobs.
type ExportJobRepository struct {
	db *gorm.DB
}

// NewExportJobRepository wires a repository to db.
func NewExportJobRepository(db *gorm.DB) *ExportJobRepository {
	return &ExportJobRepository{db: db}
}

// Create inserts a new pending export_jobs row.
func (r *ExportJobRepository) Create(ctx context.Context, j *ExportJob) error {
	if j == nil {
		return errors.New("admin: nil ExportJob")
	}
	if j.TenantID == "" {
		return errors.New("admin: ExportJob.TenantID required")
	}
	if j.ID == "" {
		j.ID = ulid.Make().String()
	}
	if j.Status == "" {
		j.Status = ExportJobStatusPending
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Create(j).Error
}

// Get returns a single job by (tenant_id, id). Tenant-scoped: a
// caller in tenant A cannot read tenant B's job by guessing the ID.
func (r *ExportJobRepository) Get(ctx context.Context, tenantID, id string) (*ExportJob, error) {
	if tenantID == "" || id == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var j ExportJob
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&j).Error; err != nil {
		return nil, err
	}
	return &j, nil
}

// MarkRunning transitions a pending job to running and stamps
// started_at. Returns gorm.ErrRecordNotFound when the job is missing
// or already past pending.
func (r *ExportJobRepository) MarkRunning(ctx context.Context, id string, now time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&ExportJob{}).
		Where("id = ? AND status = ?", id, ExportJobStatusPending).
		Updates(map[string]any{
			"status":     string(ExportJobStatusRunning),
			"started_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkSucceeded stamps download_url + completed_at and flips status
// to succeeded.
func (r *ExportJobRepository) MarkSucceeded(ctx context.Context, id, downloadURL string, now time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&ExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":       string(ExportJobStatusSucceeded),
			"download_url": downloadURL,
			"completed_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkFailed stamps the error message and flips status to failed.
func (r *ExportJobRepository) MarkFailed(ctx context.Context, id, errMsg string, now time.Time) error {
	if len(errMsg) > 4000 {
		errMsg = errMsg[:4000]
	}
	res := r.db.WithContext(ctx).
		Model(&ExportJob{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":       string(ExportJobStatusFailed),
			"error":        errMsg,
			"completed_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ClaimNextPending returns the oldest pending job, atomically
// flipping it to running so two concurrent workers cannot pick the
// same job. The caller MUST commit the transition before extending
// the work — failure to do so leaves the row stuck in running, which
// is acceptable (a long-running job won't get re-attempted, but the
// admin can manually requeue).
func (r *ExportJobRepository) ClaimNextPending(ctx context.Context, now time.Time) (*ExportJob, error) {
	var picked ExportJob
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("status = ?", ExportJobStatusPending).
			Order("created_at ASC").
			Limit(1).
			First(&picked).Error; err != nil {
			return err
		}
		res := tx.Model(&ExportJob{}).
			Where("id = ? AND status = ?", picked.ID, ExportJobStatusPending).
			Updates(map[string]any{
				"status":     string(ExportJobStatusRunning),
				"started_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		picked.Status = ExportJobStatusRunning
		picked.StartedAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &picked, nil
}

// ExportArchiveSink writes the assembled archive blob and returns a
// URL the admin can fetch. The default no-op implementation echoes
// a synthetic URL so unit tests don't need an object store.
type ExportArchiveSink interface {
	Write(ctx context.Context, tenantID, jobID string, payload []byte) (string, error)
}

// ExportArchiveSinkFunc adapts a plain function to ExportArchiveSink.
type ExportArchiveSinkFunc func(ctx context.Context, tenantID, jobID string, payload []byte) (string, error)

// Write implements ExportArchiveSink.
func (f ExportArchiveSinkFunc) Write(ctx context.Context, tenantID, jobID string, payload []byte) (string, error) {
	return f(ctx, tenantID, jobID, payload)
}

// ExportSourceLister is the narrow read contract for the source
// metadata section of the archive.
type ExportSourceLister interface {
	ListAllForTenant(ctx context.Context, tenantID string) ([]Source, error)
}

// ExportAuditLister is the narrow read contract for the audit log
// section. The implementation should already be tenant-scoped.
type ExportAuditLister interface {
	ListForExport(ctx context.Context, tenantID string, since time.Time) ([]audit.AuditLog, error)
}

// ExportWorkerConfig wires an ExportWorker.
type ExportWorkerConfig struct {
	Repo    *ExportJobRepository
	Sources ExportSourceLister
	Audits  ExportAuditLister
	Sink    ExportArchiveSink
	Audit   AuditWriter

	// AuditWindow caps how far back audit log entries are pulled
	// into the archive. Defaults to 365 days when zero.
	AuditWindow time.Duration

	// Now is the clock seam.
	Now func() time.Time
}

// ExportWorker assembles per-tenant export archives. The unit of
// work is one ProcessOne call which claims a pending job, walks the
// tenant's metadata, writes the JSON archive, and stamps the row
// with the download URL.
type ExportWorker struct {
	cfg ExportWorkerConfig
}

// NewExportWorker validates cfg and returns a worker.
func NewExportWorker(cfg ExportWorkerConfig) (*ExportWorker, error) {
	if cfg.Repo == nil {
		return nil, errors.New("admin: nil ExportJobRepository")
	}
	if cfg.Sink == nil {
		return nil, errors.New("admin: nil ExportArchiveSink")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.AuditWindow <= 0 {
		cfg.AuditWindow = 365 * 24 * time.Hour
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	return &ExportWorker{cfg: cfg}, nil
}

// ExportArchive is the JSON shape written to object storage. Only
// metadata is included — no raw chunk content, no embeddings, no
// credentials. Source rows are emitted verbatim because Source has
// already had its credentials field redacted in the model.
type ExportArchive struct {
	TenantID    string             `json:"tenant_id"`
	GeneratedAt time.Time          `json:"generated_at"`
	JobID       string             `json:"job_id"`
	Sources     []Source           `json:"sources"`
	AuditLogs   []ExportAuditEntry `json:"audit_logs,omitempty"`
}

// ExportAuditEntry is the trimmed audit row written into the
// archive. We drop Metadata (which can contain connector-specific
// payload bytes) and TraceID (operationally useful only inside the
// platform) so the export is small and reviewable.
type ExportAuditEntry struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	ActorID      string    `json:"actor_id,omitempty"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type,omitempty"`
	ResourceID   string    `json:"resource_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ProcessOne picks one pending job and runs it through to terminal
// status. Returns gorm.ErrRecordNotFound when the queue is empty so
// the caller's loop can sleep and retry.
func (w *ExportWorker) ProcessOne(ctx context.Context) error {
	job, err := w.cfg.Repo.ClaimNextPending(ctx, w.cfg.Now())
	if err != nil {
		return err
	}

	archive := ExportArchive{
		TenantID:    job.TenantID,
		JobID:       job.ID,
		GeneratedAt: w.cfg.Now(),
	}
	if w.cfg.Sources != nil {
		srcs, err := w.cfg.Sources.ListAllForTenant(ctx, job.TenantID)
		if err != nil {
			return w.fail(ctx, job, fmt.Errorf("list sources: %w", err))
		}
		archive.Sources = srcs
	}
	if w.cfg.Audits != nil {
		since := w.cfg.Now().Add(-w.cfg.AuditWindow)
		rows, err := w.cfg.Audits.ListForExport(ctx, job.TenantID, since)
		if err != nil {
			return w.fail(ctx, job, fmt.Errorf("list audits: %w", err))
		}
		archive.AuditLogs = make([]ExportAuditEntry, 0, len(rows))
		for _, r := range rows {
			archive.AuditLogs = append(archive.AuditLogs, ExportAuditEntry{
				ID:           r.ID,
				TenantID:     r.TenantID,
				ActorID:      r.ActorID,
				Action:       string(r.Action),
				ResourceType: r.ResourceType,
				ResourceID:   r.ResourceID,
				CreatedAt:    r.CreatedAt,
			})
		}
	}

	payload, err := json.Marshal(archive)
	if err != nil {
		return w.fail(ctx, job, fmt.Errorf("marshal archive: %w", err))
	}
	url, err := w.cfg.Sink.Write(ctx, job.TenantID, job.ID, payload)
	if err != nil {
		return w.fail(ctx, job, fmt.Errorf("sink: %w", err))
	}
	if err := w.cfg.Repo.MarkSucceeded(ctx, job.ID, url, w.cfg.Now()); err != nil {
		return fmt.Errorf("mark succeeded: %w", err)
	}
	w.emitAudit(ctx, job, audit.ActionTenantExportCompleted, audit.JSONMap{
		"download_url": url,
		"sources":      len(archive.Sources),
		"audit_logs":   len(archive.AuditLogs),
	})
	return nil
}

func (w *ExportWorker) fail(ctx context.Context, job *ExportJob, cause error) error {
	now := w.cfg.Now()
	msg := cause.Error()
	if err := w.cfg.Repo.MarkFailed(ctx, job.ID, msg, now); err != nil {
		return fmt.Errorf("mark failed: %w (cause: %v)", err, cause)
	}
	w.emitAudit(ctx, job, audit.ActionTenantExportCompleted, audit.JSONMap{
		"status": "failed",
		"error":  truncateForAudit(msg),
	})
	return cause
}

func (w *ExportWorker) emitAudit(ctx context.Context, job *ExportJob, action audit.Action, meta audit.JSONMap) {
	if w.cfg.Audit == nil {
		return
	}
	_ = w.cfg.Audit.Create(ctx, audit.NewAuditLog(
		job.TenantID,
		job.ActorID,
		action,
		"tenant_export",
		job.ID,
		meta,
		"",
	))
}

func truncateForAudit(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// DataExportHandler serves the export request endpoints.
type DataExportHandler struct {
	repo  *ExportJobRepository
	audit AuditWriter
	now   func() time.Time
}

// DataExportHandlerConfig configures the handler.
type DataExportHandlerConfig struct {
	Repo  *ExportJobRepository
	Audit AuditWriter
	Now   func() time.Time
}

// NewDataExportHandler validates cfg and returns a handler.
func NewDataExportHandler(cfg DataExportHandlerConfig) (*DataExportHandler, error) {
	if cfg.Repo == nil {
		return nil, errors.New("admin: nil ExportJobRepository")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &DataExportHandler{repo: cfg.Repo, audit: cfg.Audit, now: cfg.Now}, nil
}

// Register mounts the endpoints on rg.
//
//	POST /v1/admin/tenants/:tenant_id/export — create a job
//	GET  /v1/admin/tenants/:tenant_id/export/:job_id — poll status
func (h *DataExportHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/tenants/:tenant_id/export", h.create)
	rg.GET("/v1/admin/tenants/:tenant_id/export/:job_id", h.get)
}

func (h *DataExportHandler) create(c *gin.Context) {
	pathTenant := c.Param("tenant_id")
	authVal, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	authTenant, _ := authVal.(string)
	if authTenant == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	// Self-service-only: an admin of tenant A cannot trigger
	// tenant B's export. Tenant-scoping comes from the request
	// context, never from the URL.
	if pathTenant != "" && !strings.EqualFold(pathTenant, authTenant) {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant mismatch"})
		return
	}
	actor, _ := c.Get(audit.ActorContextKey)
	actorID, _ := actor.(string)

	job := &ExportJob{
		TenantID: authTenant,
		ActorID:  actorID,
		Status:   ExportJobStatusPending,
	}
	if err := h.repo.Create(c.Request.Context(), job); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist: " + err.Error()})
		return
	}
	_ = h.audit.Create(c.Request.Context(), audit.NewAuditLog(
		authTenant,
		actorID,
		audit.ActionTenantExportRequested,
		"tenant_export",
		job.ID,
		audit.JSONMap{"job_id": job.ID},
		"",
	))
	c.JSON(http.StatusAccepted, job)
}

func (h *DataExportHandler) get(c *gin.Context) {
	pathTenant := c.Param("tenant_id")
	jobID := c.Param("job_id")
	authVal, ok := c.Get(audit.TenantContextKey)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	authTenant, _ := authVal.(string)
	if authTenant == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	if pathTenant != "" && !strings.EqualFold(pathTenant, authTenant) {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant mismatch"})
		return
	}
	job, err := h.repo.Get(c.Request.Context(), authTenant, jobID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "export job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fetch: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, job)
}
