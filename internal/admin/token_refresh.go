// Package admin — token_refresh.go ships the OAuth token
// auto-refresh worker.
//
// Round-5 Task 2: connector credentials that ship a `refresh_token`
// expire on a clock — Google Drive at one hour, Box at one hour,
// Notion at no expiry but the worker still treats anything with a
// declared `expires_at` uniformly. Without a refresh worker the
// only path back to a working token is the manual rotation
// endpoint (credential_rotation.go) which requires an admin to
// paste a new ciphertext blob. This worker closes that gap by
// running on a tick: every active source whose credentials JSON
// declares (refresh_token, token_url, expires_at) and whose
// expiry falls inside RefreshWindow gets a fresh access token via
// the standard OAuth2 refresh-token grant
// (rfc6749 §6).
//
// Tenant scoping: the worker spans tenants on purpose — it is
// driven by SourceRepository.ListAllActive, not the tenant-scoped
// List. Each refresh writes back via UpdateConfig which still
// scopes by (tenant_id, id), so a misconfigured Refresh that
// returned credentials for the wrong source could not silently
// land on another tenant's row.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

// RefreshWindow is the lookahead — credentials expiring inside
// this window are refreshed eagerly. Longer than the longest
// expected RPC + retry budget so a refresh that just barely missed
// the previous tick still has time to succeed before the access
// token actually expires.
const RefreshWindow = 5 * time.Minute

// RefreshTokenStatusSuccess and friends are the labels the worker
// writes onto observability.TokenRefreshesTotal.
const (
	RefreshTokenStatusSuccess         = "success"
	RefreshTokenStatusSkipped         = "skipped"
	RefreshTokenStatusValidationError = "validation_error"
	RefreshTokenStatusTransportError  = "transport_error"
)

// TokenRefresher is the narrow contract the worker drives. The
// production wiring is OAuth2RefreshClient; tests inject a fake.
type TokenRefresher interface {
	// Refresh exchanges the supplied refresh token for a fresh
	// access token via the OAuth2 refresh-token grant. Returns
	// the new (access_token, refresh_token, expires_at). The
	// refresh_token may be unchanged when the upstream returns no
	// new one — implementations MUST echo the input refresh_token
	// in that case.
	Refresh(ctx context.Context, params RefreshParams) (RefreshResult, error)
}

// RefreshParams is the structured input the worker passes to a
// TokenRefresher. The provider URL + client credentials live in
// the credentials JSON blob alongside the access_token /
// refresh_token so the worker doesn't need a separate per-
// connector OAuth config registry.
type RefreshParams struct {
	TokenURL     string
	ClientID     string
	ClientSecret string
	RefreshToken string
	// Scope is forwarded as the `scope` parameter when set.
	Scope string
}

// RefreshResult is the data the worker writes back onto the
// source's credentials JSON.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// SourceLister is the narrow read contract the worker depends on.
// *SourceRepository satisfies it via ListAllActive.
type SourceLister interface {
	ListAllActive(ctx context.Context) ([]Source, error)
}

// SourceConfigUpdater is the narrow write contract. *SourceRepository
// satisfies it via UpdateConfig.
type SourceConfigUpdater interface {
	UpdateConfig(ctx context.Context, tenantID, id string, cfg JSONMap) error
}

// TokenRefreshWorker periodically scans every active source and
// refreshes any OAuth credential whose expiry falls inside
// RefreshWindow.
type TokenRefreshWorker struct {
	lister    SourceLister
	updater   SourceConfigUpdater
	refresher TokenRefresher
	audit     AuditWriter
	now       func() time.Time
	interval  time.Duration
	window    time.Duration
}

// TokenRefreshWorkerConfig configures the worker.
type TokenRefreshWorkerConfig struct {
	Lister    SourceLister
	Updater   SourceConfigUpdater
	Refresher TokenRefresher
	Audit     AuditWriter

	// Interval is the tick cadence. Defaults to one minute when
	// zero — refresh windows are minute-scale so tighter ticks add
	// no signal.
	Interval time.Duration

	// Window overrides RefreshWindow (test seam).
	Window time.Duration

	// Now is the clock seam. Defaults to time.Now.UTC.
	Now func() time.Time
}

// NewTokenRefreshWorker validates cfg and returns a worker.
func NewTokenRefreshWorker(cfg TokenRefreshWorkerConfig) (*TokenRefreshWorker, error) {
	if cfg.Lister == nil {
		return nil, errors.New("admin: nil SourceLister")
	}
	if cfg.Updater == nil {
		return nil, errors.New("admin: nil SourceConfigUpdater")
	}
	if cfg.Refresher == nil {
		return nil, errors.New("admin: nil TokenRefresher")
	}
	w := &TokenRefreshWorker{
		lister:    cfg.Lister,
		updater:   cfg.Updater,
		refresher: cfg.Refresher,
		audit:     cfg.Audit,
		interval:  cfg.Interval,
		window:    cfg.Window,
		now:       cfg.Now,
	}
	if w.audit == nil {
		w.audit = noopAudit{}
	}
	if w.interval <= 0 {
		w.interval = time.Minute
	}
	if w.window <= 0 {
		w.window = RefreshWindow
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// Run drives the worker until ctx is cancelled. The first scan
// fires immediately so a deployment that just rolled through a
// long quiet window catches up without waiting a full tick.
func (w *TokenRefreshWorker) Run(ctx context.Context) error {
	if _, err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// First-tick failure is logged via the metric and the
		// per-source audit row; the loop continues.
		_ = err
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := w.Tick(ctx); err != nil && errors.Is(err, context.Canceled) {
				return err
			}
		}
	}
}

// TickStats summarises one Tick run for tests.
type TickStats struct {
	Scanned   int
	Refreshed int
	Skipped   int
	Errors    int
}

// Tick scans every active source once and refreshes those whose
// access tokens are inside the refresh window. The function
// returns when every source has been considered. Errors are
// per-source and recorded on the metric; only an error from
// ListAllActive aborts the tick.
func (w *TokenRefreshWorker) Tick(ctx context.Context) (TickStats, error) {
	srcs, err := w.lister.ListAllActive(ctx)
	if err != nil {
		return TickStats{}, fmt.Errorf("admin: token refresh tick: %w", err)
	}
	stats := TickStats{Scanned: len(srcs)}
	now := w.now()
	for i := range srcs {
		s := &srcs[i]
		creds, ok := readOAuthCredentials(s.Config)
		if !ok {
			stats.Skipped++
			continue
		}
		if creds.ExpiresAt.IsZero() || creds.ExpiresAt.Sub(now) > w.window {
			stats.Skipped++
			observability.TokenRefreshesTotal.WithLabelValues(s.ConnectorType, RefreshTokenStatusSkipped).Inc()
			continue
		}
		res, rerr := w.refresher.Refresh(ctx, RefreshParams{
			TokenURL:     creds.TokenURL,
			ClientID:     creds.ClientID,
			ClientSecret: creds.ClientSecret,
			RefreshToken: creds.RefreshToken,
			Scope:        creds.Scope,
		})
		if rerr != nil {
			stats.Errors++
			label := RefreshTokenStatusTransportError
			var ve *RefreshValidationError
			if errors.As(rerr, &ve) {
				label = RefreshTokenStatusValidationError
			}
			observability.TokenRefreshesTotal.WithLabelValues(s.ConnectorType, label).Inc()
			continue
		}
		// Persist the refreshed access token + expiry. Preserve
		// every other key on the config blob — the worker is the
		// surgical owner of the OAuth fields only.
		newCfg := cloneConfig(s.Config)
		credsBlob := credentialsFromConfig(newCfg)
		credsBlob["access_token"] = res.AccessToken
		if res.RefreshToken != "" {
			credsBlob["refresh_token"] = res.RefreshToken
		}
		credsBlob["expires_at"] = res.ExpiresAt.UTC().Format(time.RFC3339Nano)
		newCfg["credentials"] = credsBlob
		newCfg["credentials_rotated_at"] = now.Format(time.RFC3339Nano)

		if err := w.updater.UpdateConfig(ctx, s.TenantID, s.ID, newCfg); err != nil {
			stats.Errors++
			observability.TokenRefreshesTotal.WithLabelValues(s.ConnectorType, RefreshTokenStatusTransportError).Inc()
			continue
		}
		stats.Refreshed++
		observability.TokenRefreshesTotal.WithLabelValues(s.ConnectorType, RefreshTokenStatusSuccess).Inc()
		_ = w.audit.Create(ctx, audit.NewAuditLog(
			s.TenantID, "system:token_refresh", audit.ActionSourceTokenRefreshed,
			"source", s.ID, audit.JSONMap{
				"connector":  s.ConnectorType,
				"expires_at": res.ExpiresAt.UTC().Format(time.RFC3339Nano),
			}, "",
		))
	}
	return stats, nil
}

// oauthCredentials is the typed view of the credentials blob we
// expect on OAuth-based connectors. The worker tolerates fields
// being absent (the source is then "skipped") so a non-OAuth
// connector lands on the same code path without silently
// erroring.
type oauthCredentials struct {
	AccessToken  string
	RefreshToken string
	TokenURL     string
	ClientID     string
	ClientSecret string
	Scope        string
	ExpiresAt    time.Time
}

func readOAuthCredentials(cfg JSONMap) (oauthCredentials, bool) {
	c := credentialsFromConfig(cfg)
	if c == nil {
		return oauthCredentials{}, false
	}
	out := oauthCredentials{
		AccessToken:  stringField(c, "access_token"),
		RefreshToken: stringField(c, "refresh_token"),
		TokenURL:     stringField(c, "token_url"),
		ClientID:     stringField(c, "client_id"),
		ClientSecret: stringField(c, "client_secret"),
		Scope:        stringField(c, "scope"),
	}
	if out.RefreshToken == "" || out.TokenURL == "" {
		return oauthCredentials{}, false
	}
	if v, ok := c["expires_at"]; ok {
		if s, _ := v.(string); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				out.ExpiresAt = t
			} else if t, err := time.Parse(time.RFC3339, s); err == nil {
				out.ExpiresAt = t
			}
		}
	}
	return out, true
}

// credentialsFromConfig pulls the credentials sub-map out of the
// source config, normalising both the JSONMap and bare map[string]any
// shapes GORM emits depending on the driver.
func credentialsFromConfig(cfg JSONMap) map[string]any {
	if cfg == nil {
		return nil
	}
	raw, ok := cfg["credentials"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case map[string]any:
		// Normalise to a fresh map so callers can safely mutate.
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[k] = vv
		}
		return out
	case JSONMap:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[k] = vv
		}
		return out
	case []byte:
		var m map[string]any
		if err := json.Unmarshal(v, &m); err != nil {
			return nil
		}
		return m
	case string:
		// SQLite + glebarez round-trips JSONB as strings; tolerate
		// the non-Postgres form so unit tests with the in-memory
		// repo fixture exercise the same parser.
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err != nil {
			return nil
		}
		return m
	}
	return nil
}

// cloneConfig returns a deep copy of cfg so the worker can mutate
// the cloned blob (rotating access_token, refresh_token,
// expires_at, credentials_rotated_at) without aliasing the
// in-memory representation of the source's config that callers
// may still hold a pointer to.
//
// Addresses FLAG_pr-review-job-b10ff0a8305841f98c7f1ed361d5ee8b_0007:
// the previous implementation copied only the top-level keys, so
// nested maps (notably `credentials`) and slices were shared
// between the clone and the original. The current call sites
// happen to substitute the credentials sub-map wholesale, which
// hid the bug, but a future caller that did
// `newCfg["credentials"].(map[string]any)["access_token"] = ...`
// would have silently mutated s.Config in place. The deep clone
// closes that footgun.
func cloneConfig(cfg JSONMap) JSONMap {
	out := make(JSONMap, len(cfg))
	for k, v := range cfg {
		out[k] = deepCopyJSONValue(v)
	}
	return out
}

// deepCopyJSONValue recursively clones a JSON-shaped value. Only
// the shapes that survive a JSONB round-trip are honoured; pointer
// types, channels, funcs, etc. should never appear in cfg blobs and
// would be a programming error if they did.
func deepCopyJSONValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case JSONMap:
		out := make(JSONMap, len(t))
		for k, vv := range t {
			out[k] = deepCopyJSONValue(vv)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyJSONValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyJSONValue(vv)
		}
		return out
	case []byte:
		// JSON-encoded bytes are sometimes round-tripped as
		// raw byte slices; copy so the clone owns its buffer.
		out := make([]byte, len(t))
		copy(out, t)
		return out
	default:
		// Strings, numbers, bools — value types with no
		// aliasing to worry about.
		return t
	}
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// RefreshValidationError signals that the OAuth provider rejected
// the refresh request (HTTP 4xx, malformed body). The worker
// distinguishes these from transport errors so the metric label
// is meaningful and an alert can branch on the cause.
type RefreshValidationError struct {
	Err error
}

func (e *RefreshValidationError) Error() string {
	if e == nil || e.Err == nil {
		return "refresh validation error"
	}
	return "refresh validation error: " + e.Err.Error()
}

func (e *RefreshValidationError) Unwrap() error { return e.Err }

// OAuth2RefreshClient is the production TokenRefresher. It POSTs
// a x-www-form-urlencoded body to the connector-supplied
// `token_url` carrying the refresh-token grant exactly as
// described in RFC 6749 §6 and unpacks the JSON response.
type OAuth2RefreshClient struct {
	HTTPClient *http.Client
	Now        func() time.Time
}

// NewOAuth2RefreshClient returns a client with sensible defaults.
func NewOAuth2RefreshClient(c *http.Client) *OAuth2RefreshClient {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &OAuth2RefreshClient{HTTPClient: c}
}

// Refresh implements TokenRefresher.
func (c *OAuth2RefreshClient) Refresh(ctx context.Context, p RefreshParams) (RefreshResult, error) {
	if p.TokenURL == "" {
		return RefreshResult{}, &RefreshValidationError{Err: errors.New("token_url required")}
	}
	if p.RefreshToken == "" {
		return RefreshResult{}, &RefreshValidationError{Err: errors.New("refresh_token required")}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", p.RefreshToken)
	if p.ClientID != "" {
		form.Set("client_id", p.ClientID)
	}
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	if p.Scope != "" {
		form.Set("scope", p.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return RefreshResult{}, fmt.Errorf("oauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return RefreshResult{}, fmt.Errorf("oauth: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		ExpiresAt    string `json:"expires_at"`
		Error        string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		return RefreshResult{}, fmt.Errorf("oauth: decode response: %w", err)
	}
	if resp.StatusCode >= 400 || body.Error != "" {
		msg := body.Error
		if msg == "" {
			msg = "http " + strconv.Itoa(resp.StatusCode)
		}
		return RefreshResult{}, &RefreshValidationError{Err: errors.New(msg)}
	}
	if body.AccessToken == "" {
		return RefreshResult{}, &RefreshValidationError{Err: errors.New("response missing access_token")}
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	expiresAt := time.Time{}
	switch {
	case body.ExpiresAt != "":
		if t, err := time.Parse(time.RFC3339Nano, body.ExpiresAt); err == nil {
			expiresAt = t
		} else if t, err := time.Parse(time.RFC3339, body.ExpiresAt); err == nil {
			expiresAt = t
		}
	case body.ExpiresIn > 0:
		expiresAt = now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	rt := body.RefreshToken
	if rt == "" {
		rt = p.RefreshToken
	}
	return RefreshResult{
		AccessToken:  body.AccessToken,
		RefreshToken: rt,
		ExpiresAt:    expiresAt,
	}, nil
}
