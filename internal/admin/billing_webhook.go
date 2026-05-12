package admin

// billing_webhook.go — Round-19 Task 26.
//
// Daily tenant-usage billing webhook. A background worker walks
// the metering store at a configurable interval, aggregates per-
// tenant usage for the previous UTC day, and dispatches a single
// JSON POST per tenant to the configured webhook URL.
//
// Design constraints:
//
//   - Gated behind CONTEXT_ENGINE_BILLING_WEBHOOK_URL — when the
//     env var is unset the worker never starts.
//   - Stdlib HTTP only. The webhook is at most one request per
//     tenant per day; no exotic transport requirements.
//   - Per-tenant error isolation. A failing webhook for one
//     tenant must not block the rest of the batch.
//   - Idempotency via a Worker-supplied X-Idempotency-Key header
//     so the downstream billing system can de-dupe retries.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// BillingTenantLister narrows the dependency the worker needs from
// the tenant directory: enumerate every active tenant.
type BillingTenantLister interface {
	ListTenantIDs(ctx context.Context) ([]string, error)
}

// BillingUsageReader narrows MeteringStore to the read surface the
// worker needs.
type BillingUsageReader interface {
	List(ctx context.Context, tenantID string, from, to time.Time) ([]TenantUsage, error)
}

// BillingWebhookConfig configures the dispatcher.
type BillingWebhookConfig struct {
	// URL is the destination billing webhook. Mirrors
	// CONTEXT_ENGINE_BILLING_WEBHOOK_URL. Required.
	URL string

	// HTTPClient is swappable for tests. Defaults to a 30-second
	// timeout client.
	HTTPClient *http.Client

	// AuthHeader, if set, is sent as the Authorization header on
	// the webhook POST.
	AuthHeader string

	// Now is the wall-clock provider, swappable for tests.
	// Defaults to time.Now.
	Now func() time.Time
}

// BillingWebhookDispatcher dispatches per-tenant daily usage to
// the configured webhook.
type BillingWebhookDispatcher struct {
	cfg     BillingWebhookConfig
	tenants BillingTenantLister
	usage   BillingUsageReader
}

// NewBillingWebhookDispatcher validates inputs.
func NewBillingWebhookDispatcher(cfg BillingWebhookConfig, tenants BillingTenantLister, usage BillingUsageReader) (*BillingWebhookDispatcher, error) {
	if cfg.URL == "" {
		return nil, errors.New("billing-webhook: URL is required")
	}
	if tenants == nil {
		return nil, errors.New("billing-webhook: nil tenant lister")
	}
	if usage == nil {
		return nil, errors.New("billing-webhook: nil usage reader")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	return &BillingWebhookDispatcher{cfg: cfg, tenants: tenants, usage: usage}, nil
}

// BillingWebhookPayload is the JSON shape the dispatcher POSTs.
// Exported so downstream billing systems can pin the schema.
type BillingWebhookPayload struct {
	TenantID string         `json:"tenant_id"`
	Day      string         `json:"day"`
	Usage    map[string]int64 `json:"usage"`
}

// DispatchOnce runs the worker exactly once for the day preceding
// `at` (UTC). Returns the number of tenants successfully POSTed
// plus the first encountered error (if any) so the operator log
// can surface it.
func (d *BillingWebhookDispatcher) DispatchOnce(ctx context.Context, at time.Time) (int, error) {
	day := truncateToUTCDay(at.UTC()).Add(-24 * time.Hour)
	tenants, err := d.tenants.ListTenantIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("billing-webhook: list tenants: %w", err)
	}
	var firstErr error
	dispatched := 0
	for _, tid := range tenants {
		ok, err := d.dispatchOne(ctx, tid, day)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}

			continue
		}
		if ok {
			dispatched++
		}
	}

	return dispatched, firstErr
}

func (d *BillingWebhookDispatcher) dispatchOne(ctx context.Context, tenantID string, day time.Time) (bool, error) {
	end := day.Add(24 * time.Hour)
	rows, err := d.usage.List(ctx, tenantID, day, end)
	if err != nil {
		return false, fmt.Errorf("billing-webhook: usage for %s: %w", tenantID, err)
	}
	if len(rows) == 0 {
		return false, nil
	}
	agg := map[string]int64{}
	for _, r := range rows {
		agg[r.Metric] += r.Count
	}
	payload := BillingWebhookPayload{
		TenantID: tenantID,
		Day:      day.Format("2006-01-02"),
		Usage:    agg,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("billing-webhook: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("billing-webhook: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", fmt.Sprintf("billing:%s:%s", tenantID, payload.Day))
	if d.cfg.AuthHeader != "" {
		req.Header.Set("Authorization", d.cfg.AuthHeader)
	}
	resp, err := d.cfg.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("billing-webhook: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("billing-webhook: %s -> %d", tenantID, resp.StatusCode)
	}

	return true, nil
}

// truncateToUTCDay returns t with the time-of-day component zeroed.
func truncateToUTCDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
