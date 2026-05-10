// credential_monitor.go — Round-5 Task 14.
//
// Background goroutine that runs every CredentialMonitorInterval and
// checks each active source's credentials for approaching or past
// expiry. When a credential is within CredentialExpiryWarningWindow
// the worker emits `source.credential_expiring` + a Prometheus
// gauge. When it has expired the worker emits
// `source.credential_expired` and marks the source health as
// degraded.
//
// The worker spans all tenants via ListAllActive — it is an
// operational surface, not tenant-scoped. Dedup is in-memory:
// each (tenant, source) pair transitions through states
// (ok → expiring → expired) so audit events fire at most once per
// transition. A worker restart resets the state, which means a
// second audit event for the same (tenant, source) — acceptable
// because the audit log is append-only and the admin portal
// de-dupes by resource_id on the timeline.
package admin

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// CredentialMonitorInterval is the default run frequency. One hour
// strikes a balance between timely detection (sub-8h warning
// window) and DB load (one SELECT-all-active per hour).
const CredentialMonitorInterval = time.Hour

// CredentialExpiryWarningWindow is how far ahead the worker looks.
// Seven days matches the calendar cadence most SRE rotations
// follow.
const CredentialExpiryWarningWindow = 7 * 24 * time.Hour

// CredentialMonitorSourceLister narrows SourceRepository to the
// cross-tenant ListAllActive path.
type CredentialMonitorSourceLister interface {
	ListAllActive(ctx context.Context) ([]Source, error)
}

// CredentialMonitorHealthWriter abstracts the minimal health-update
// path so tests can inject a fake without wiring a full
// HealthRepository.
type CredentialMonitorHealthWriter interface {
	RecordFailure(ctx context.Context, tenantID, sourceID string) error
}

type credState int

const (
	credOK       credState = 0
	credExpiring credState = 1
	credExpired  credState = 2
)

// CredentialMonitorConfig configures the worker.
type CredentialMonitorConfig struct {
	Lister CredentialMonitorSourceLister
	Health CredentialMonitorHealthWriter
	Audit  AuditWriter
	Logger *slog.Logger
	Now    func() time.Time // test seam; nil → time.Now().UTC()
}

// CredentialMonitor is the background worker.
type CredentialMonitor struct {
	cfg  CredentialMonitorConfig
	seen map[string]credState
}

// NewCredentialMonitor validates cfg and returns the worker.
func NewCredentialMonitor(cfg CredentialMonitorConfig) (*CredentialMonitor, error) {
	if cfg.Lister == nil {
		return nil, errors.New("credential_monitor: nil Lister")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &CredentialMonitor{cfg: cfg, seen: map[string]credState{}}, nil
}

// Tick runs one pass of the monitor. Called by a ticker in
// production; called directly in tests.
func (m *CredentialMonitor) Tick(ctx context.Context) {
	now := m.now()
	srcs, err := m.cfg.Lister.ListAllActive(ctx)
	if err != nil {
		m.cfg.Logger.Error("credential_monitor: list", "error", err)
		return
	}
	for i := range srcs {
		m.checkSource(ctx, &srcs[i], now)
	}
}

func (m *CredentialMonitor) now() time.Time {
	if m.cfg.Now != nil {
		return m.cfg.Now()
	}
	return time.Now().UTC()
}

func (m *CredentialMonitor) checkSource(ctx context.Context, s *Source, now time.Time) {
	expiresAt, ok := credentialExpiresAt(s)
	if !ok {
		return
	}
	key := s.TenantID + "|" + s.ID
	remaining := expiresAt.Sub(now)
	switch {
	case remaining <= 0:
		if m.seen[key] < credExpired {
			m.seen[key] = credExpired
			_ = m.cfg.Audit.Create(ctx, audit.NewAuditLog(
				s.TenantID, "system", audit.ActionSourceCredentialExpired, "source", s.ID,
				audit.JSONMap{"connector": s.ConnectorType, "expires_at": expiresAt.Format(time.RFC3339)}, "",
			))
			observability.CredentialsExpiring.WithLabelValues(s.ConnectorType, s.ID).Set(1)
			if m.cfg.Health != nil {
				_ = m.cfg.Health.RecordFailure(ctx, s.TenantID, s.ID)
			}
		}
	case remaining < CredentialExpiryWarningWindow:
		if m.seen[key] < credExpiring {
			m.seen[key] = credExpiring
			_ = m.cfg.Audit.Create(ctx, audit.NewAuditLog(
				s.TenantID, "system", audit.ActionSourceCredentialExpiring, "source", s.ID,
				audit.JSONMap{
					"connector":    s.ConnectorType,
					"expires_at":   expiresAt.Format(time.RFC3339),
					"remaining_hr": int(remaining.Hours()),
				}, "",
			))
			observability.CredentialsExpiring.WithLabelValues(s.ConnectorType, s.ID).Set(1)
		}
	default:
		if m.seen[key] != credOK {
			m.seen[key] = credOK
			observability.CredentialsExpiring.WithLabelValues(s.ConnectorType, s.ID).Set(0)
		}
	}
}

// credentialExpiresAt extracts the credential expiry from the
// source's config blob. Returns the zero time if the credential
// doesn't carry an expires_at (e.g. API-key connectors).
func credentialExpiresAt(s *Source) (time.Time, bool) {
	if s == nil || s.Config == nil {
		return time.Time{}, false
	}
	creds, ok := s.Config["credentials"]
	if !ok {
		return time.Time{}, false
	}
	credsMap, ok := creds.(map[string]any)
	if !ok {
		return time.Time{}, false
	}
	raw, ok := credsMap["expires_at"]
	if !ok {
		return time.Time{}, false
	}
	str, ok := raw.(string)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, str)
	if err != nil {
		t, err = time.Parse(time.RFC3339, str)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}
