package admin_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeMonitorLister struct {
	srcs []admin.Source
}

func (f *fakeMonitorLister) ListAllActive(_ context.Context) ([]admin.Source, error) {
	return f.srcs, nil
}

type fakeHealthRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeHealthRecorder) RecordFailure(_ context.Context, tenantID, sourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, tenantID+"/"+sourceID)
	return nil
}

func newSourceWithExpiry(tenantID, id, connector string, expiresAt time.Time) admin.Source {
	cfg := admin.JSONMap{
		"credentials": map[string]any{
			"access_token": "tok",
			"expires_at":   expiresAt.Format(time.RFC3339Nano),
		},
	}
	return admin.Source{
		ID: id, TenantID: tenantID, ConnectorType: connector,
		Config: cfg, Status: admin.SourceStatusActive,
	}
}

func TestCredentialMonitor_FreshCredentialNoOp(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	src := newSourceWithExpiry("tenant-a", "src-1", "google_drive", now.Add(30*24*time.Hour))
	au := &auditCapture{}
	mon, err := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
		Audit:  au,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewCredentialMonitor: %v", err)
	}
	mon.Tick(context.Background())
	if len(au.Calls()) != 0 {
		t.Fatalf("fresh credential should not emit audit, got %d", len(au.Calls()))
	}
}

func TestCredentialMonitor_ExpiringEmitsAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	src := newSourceWithExpiry("tenant-a", "src-2", "google_drive", now.Add(48*time.Hour))
	au := &auditCapture{}
	mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
		Audit:  au,
		Now:    func() time.Time { return now },
	})
	mon.Tick(context.Background())
	calls := au.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 audit call, got %d", len(calls))
	}
	if calls[0].Action != audit.ActionSourceCredentialExpiring {
		t.Fatalf("audit action: %q", calls[0].Action)
	}
}

func TestCredentialMonitor_ExpiredEmitsAuditAndDegradesHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	src := newSourceWithExpiry("tenant-a", "src-3", "slack", now.Add(-time.Hour))
	au := &auditCapture{}
	hr := &fakeHealthRecorder{}
	mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
		Audit:  au,
		Health: hr,
		Now:    func() time.Time { return now },
	})
	mon.Tick(context.Background())
	calls := au.Calls()
	if len(calls) != 1 || calls[0].Action != audit.ActionSourceCredentialExpired {
		t.Fatalf("unexpected audit: %+v", calls)
	}
	hr.mu.Lock()
	defer hr.mu.Unlock()
	if len(hr.calls) != 1 || hr.calls[0] != "tenant-a/src-3" {
		t.Fatalf("expected RecordFailure for src-3, got %v", hr.calls)
	}
}

func TestCredentialMonitor_DedupAcrossTicks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	src := newSourceWithExpiry("tenant-a", "src-4", "google_drive", now.Add(48*time.Hour))
	au := &auditCapture{}
	mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
		Audit:  au,
		Now:    func() time.Time { return now },
	})
	mon.Tick(context.Background())
	mon.Tick(context.Background())
	mon.Tick(context.Background())
	if len(au.Calls()) != 1 {
		t.Fatalf("expected 1 dedup audit, got %d", len(au.Calls()))
	}
}

func TestCredentialMonitor_TransitionExpiringToExpiredEmitsBoth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	src := newSourceWithExpiry("tenant-a", "src-5", "google_drive", now.Add(48*time.Hour))
	lister := &fakeMonitorLister{srcs: []admin.Source{src}}
	au := &auditCapture{}
	clock := now
	mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: lister,
		Audit:  au,
		Now:    func() time.Time { return clock },
	})
	// First tick → expiring.
	mon.Tick(context.Background())
	// Advance clock past expiry → next tick should fire expired.
	clock = now.Add(72 * time.Hour)
	mon.Tick(context.Background())
	calls := au.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 audits (expiring then expired), got %d", len(calls))
	}
	if calls[0].Action != audit.ActionSourceCredentialExpiring ||
		calls[1].Action != audit.ActionSourceCredentialExpired {
		t.Fatalf("unexpected audit sequence: %+v", calls)
	}
}

func TestCredentialMonitor_NoExpiresAtSkipped(t *testing.T) {
	t.Parallel()
	src := admin.Source{
		ID: "src-6", TenantID: "tenant-a", ConnectorType: "api_key",
		Config: admin.JSONMap{"credentials": map[string]any{"api_key": "k"}},
		Status: admin.SourceStatusActive,
	}
	au := &auditCapture{}
	mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
		Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
		Audit:  au,
	})
	mon.Tick(context.Background())
	if len(au.Calls()) != 0 {
		t.Fatalf("no expires_at means no audit, got %d", len(au.Calls()))
	}
}

func TestCredentialMonitor_NilListerErrors(t *testing.T) {
	t.Parallel()
	_, err := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{Audit: &auditCapture{}})
	if err == nil {
		t.Fatalf("expected error for nil Lister")
	}
}
