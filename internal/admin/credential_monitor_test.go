package admin_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// gaugeValue extracts the current value of a labelled gauge for
// regression-asserting the credential expiry metric.
func gaugeValue(t *testing.T, connector, sourceID string) float64 {
	t.Helper()
	g, err := observability.CredentialsExpiring.GetMetricWithLabelValues(connector, sourceID)
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	m := &dto.Metric{}
	if err := g.Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if m.Gauge == nil || m.Gauge.Value == nil {
		t.Fatalf("nil gauge value")
	}
	return *m.Gauge.Value
}

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

// TestCredentialMonitor_GaugePublishesActualDays regression-tests
// the BUG_pr-review-job-..._0003 fix: the
// context_engine_credentials_expiring_days gauge MUST publish the
// real day count, not a 0/1 boolean. Alerting rules in the
// runbook (docs/runbooks/alerting.md:115) threshold on
// "remaining < 7 days" and would never fire if the gauge stayed
// at 0/1.
func TestCredentialMonitor_GaugePublishesActualDays(t *testing.T) {
	// Cannot t.Parallel: shares the package-level
	// observability.CredentialsExpiring registry across cases.
	cases := []struct {
		name     string
		offset   time.Duration
		minDays  float64
		maxDays  float64
		negative bool
	}{
		{name: "fresh", offset: 30 * 24 * time.Hour, minDays: 29.5, maxDays: 30.5},
		{name: "expiring", offset: 48 * time.Hour, minDays: 1.5, maxDays: 2.5},
		{name: "expired", offset: -25 * time.Hour, minDays: -1.5, maxDays: -1.0, negative: true},
	}
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := "metric-" + tc.name
			src := newSourceWithExpiry("tenant-m", id, "google_drive", now.Add(tc.offset))
			au := &auditCapture{}
			mon, _ := admin.NewCredentialMonitor(admin.CredentialMonitorConfig{
				Lister: &fakeMonitorLister{srcs: []admin.Source{src}},
				Audit:  au,
				Now:    func() time.Time { return now },
			})
			mon.Tick(context.Background())
			got := gaugeValue(t, "google_drive", id)
			if got < tc.minDays || got > tc.maxDays {
				t.Fatalf("case %d (%s): gauge=%v want in [%v, %v]",
					i, tc.name, got, tc.minDays, tc.maxDays)
			}
			if tc.negative && got >= 0 {
				t.Fatalf("expired credential must publish negative days, got %v", got)
			}
			// 0 or 1 would mean the bug is back; ensure value is
			// distinct from those sentinels for non-trivial offsets.
			if math.Abs(got-1) < 0.01 || math.Abs(got) < 0.01 {
				t.Fatalf("gauge looks boolean (got %v) — BUG_0003 regression?", got)
			}
		})
	}
}
