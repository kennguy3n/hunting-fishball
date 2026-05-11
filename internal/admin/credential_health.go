// credential_health.go — Round-7 Task 7.
//
// Periodically calls `connector.Validate(ctx, cfg)` for every
// active source and records the result in source_health.
// credential_valid + credential_checked_at + credential_error.
//
// The worker emits `source.credential_invalid` audit events the
// first time a source transitions from valid → invalid and flips
// the source_health.credential_valid column so the admin portal
// can surface the bad credential.
//
// Operationally this is the proactive sibling of the Round-5
// credential_monitor: that worker watches the expiry timestamp;
// this one actually exercises the credential against the upstream
// API. Both write to source_health and emit distinct audit
// actions so the admin timeline can distinguish them.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// CredentialHealthInterval is the default tick frequency. Six
// hours strikes a balance between catching newly-revoked tokens
// and not hammering upstream APIs.
const CredentialHealthInterval = 6 * time.Hour

// CredentialValidator narrows SourceConnector to just Validate so
// tests can inject a fake without standing up a full connector.
type CredentialValidator interface {
	Validate(ctx context.Context, connectorType string, cfg connector.ConnectorConfig) error
}

// CredentialHealthWriter writes the result of a single validation
// pass into source_health.
type CredentialHealthWriter interface {
	RecordCredentialCheck(ctx context.Context, tenantID, sourceID string, valid bool, message string) error
}

// CredentialHealthConfig wires the worker.
type CredentialHealthConfig struct {
	Lister    CredentialMonitorSourceLister
	Validator CredentialValidator
	Health    CredentialHealthWriter
	Audit     AuditWriter
	Logger    *slog.Logger
	Now       func() time.Time
}

// CredentialHealthWorker is the periodic background worker.
type CredentialHealthWorker struct {
	cfg  CredentialHealthConfig
	mu   sync.Mutex
	prev map[string]bool // key = tenantID|sourceID
}

// NewCredentialHealthWorker validates cfg.
func NewCredentialHealthWorker(cfg CredentialHealthConfig) (*CredentialHealthWorker, error) {
	if cfg.Lister == nil {
		return nil, errors.New("credential_health: nil Lister")
	}
	if cfg.Validator == nil {
		return nil, errors.New("credential_health: nil Validator")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &CredentialHealthWorker{cfg: cfg, prev: map[string]bool{}}, nil
}

// Tick runs one validation pass across every active source.
func (w *CredentialHealthWorker) Tick(ctx context.Context) {
	srcs, err := w.cfg.Lister.ListAllActive(ctx)
	if err != nil {
		w.cfg.Logger.Error("credential_health: list", "error", err)
		return
	}
	for i := range srcs {
		w.checkSource(ctx, &srcs[i])
	}
}

func (w *CredentialHealthWorker) checkSource(ctx context.Context, s *Source) {
	cfg := connector.ConnectorConfig{
		Name:     s.ConnectorType,
		TenantID: s.TenantID,
		SourceID: s.ID,
	}
	err := w.cfg.Validator.Validate(ctx, s.ConnectorType, cfg)
	key := s.TenantID + "|" + s.ID
	w.mu.Lock()
	prev, seen := w.prev[key]
	w.prev[key] = err == nil
	w.mu.Unlock()
	valid := err == nil
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if w.cfg.Health != nil {
		_ = w.cfg.Health.RecordCredentialCheck(ctx, s.TenantID, s.ID, valid, msg)
	}
	// Audit on transitions: invalid → ok or ok → invalid.
	if seen && prev == valid {
		return
	}
	if !valid {
		_ = w.cfg.Audit.Create(ctx, audit.NewAuditLog(
			s.TenantID, "system", audit.ActionSourceCredentialInvalid, "source", s.ID,
			audit.JSONMap{"connector": s.ConnectorType, "error": msg}, "",
		))
	}
}

// InMemoryCredentialHealthStore is a goroutine-safe map-backed
// store used by tests and the admin endpoint when no Postgres
// connection is wired.
type InMemoryCredentialHealthStore struct {
	mu     sync.RWMutex
	checks map[string]CredentialCheckRow
}

// CredentialCheckRow is the projected health row.
type CredentialCheckRow struct {
	TenantID  string    `json:"tenant_id"`
	SourceID  string    `json:"source_id"`
	Valid     bool      `json:"credential_valid"`
	Message   string    `json:"credential_error,omitempty"`
	CheckedAt time.Time `json:"credential_checked_at"`
}

// NewInMemoryCredentialHealthStore builds the fake.
func NewInMemoryCredentialHealthStore() *InMemoryCredentialHealthStore {
	return &InMemoryCredentialHealthStore{checks: map[string]CredentialCheckRow{}}
}

// RecordCredentialCheck implements CredentialHealthWriter.
func (s *InMemoryCredentialHealthStore) RecordCredentialCheck(_ context.Context, tenantID, sourceID string, valid bool, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[tenantID+"|"+sourceID] = CredentialCheckRow{
		TenantID: tenantID, SourceID: sourceID,
		Valid: valid, Message: message,
		CheckedAt: time.Now().UTC(),
	}
	return nil
}

// Get returns the most recent recorded check for (tenant, source).
func (s *InMemoryCredentialHealthStore) Get(_ context.Context, tenantID, sourceID string) (CredentialCheckRow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.checks[tenantID+"|"+sourceID]
	return r, ok
}

// CredentialHealthReader exposes the projected row to the admin
// endpoint.
type CredentialHealthReader interface {
	Get(ctx context.Context, tenantID, sourceID string) (CredentialCheckRow, bool)
}

// CredentialHealthHandler serves
// GET /v1/admin/sources/:id/credential-health.
type CredentialHealthHandler struct {
	store CredentialHealthReader
}

// NewCredentialHealthHandler validates inputs.
func NewCredentialHealthHandler(store CredentialHealthReader) (*CredentialHealthHandler, error) {
	if store == nil {
		return nil, errors.New("credential_health: nil store")
	}
	return &CredentialHealthHandler{store: store}, nil
}

// Register mounts the route.
func (h *CredentialHealthHandler) Register(rg *gin.RouterGroup) {
	rg.GET("/v1/admin/sources/:id/credential-health", h.get)
}

func (h *CredentialHealthHandler) get(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	row, found := h.store.Get(c.Request.Context(), tenantID, c.Param("id"))
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "no credential health recorded"})
		return
	}
	c.JSON(http.StatusOK, row)
}
