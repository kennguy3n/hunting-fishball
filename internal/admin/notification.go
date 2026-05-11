package admin

// notification.go — Round-6 Task 11.
//
// Admin notification preferences. Operators register a webhook URL
// (or email address) for one or more audit event types; the audit
// pipeline calls Dispatch() and the configured notifier(s) deliver
// the payload. Channels are "webhook" (HTTP POST) or "email"
// (handled outside this package).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// NotificationChannel enumerates the supported delivery channels.
type NotificationChannel string

const (
	// NotificationChannelWebhook delivers via HTTP POST.
	NotificationChannelWebhook NotificationChannel = "webhook"
	// NotificationChannelEmail delivers via the email gateway.
	NotificationChannelEmail NotificationChannel = "email"
)

// NotificationPreference is the persisted shape.
type NotificationPreference struct {
	ID        string              `json:"id"`
	TenantID  string              `json:"tenant_id"`
	EventType string              `json:"event_type"`
	Channel   NotificationChannel `json:"channel"`
	Target    string              `json:"target"`
	Enabled   bool                `json:"enabled"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// Validate returns nil when n is well-formed.
func (n *NotificationPreference) Validate() error {
	if n == nil {
		return errors.New("notification: nil preference")
	}
	if n.TenantID == "" {
		return errors.New("notification: missing tenant_id")
	}
	if n.EventType == "" {
		return errors.New("notification: missing event_type")
	}
	if n.Target == "" {
		return errors.New("notification: missing target")
	}
	switch n.Channel {
	case NotificationChannelWebhook, NotificationChannelEmail:
	default:
		return errors.New("notification: invalid channel")
	}
	return nil
}

// NotificationStore is the persistence port.
type NotificationStore interface {
	List(tenantID string) ([]*NotificationPreference, error)
	ListByEvent(tenantID, eventType string) ([]*NotificationPreference, error)
	Create(*NotificationPreference) error
	Delete(tenantID, id string) error
}

// InMemoryNotificationStore is a goroutine-safe map-backed
// implementation used by tests.
type InMemoryNotificationStore struct {
	mu   sync.RWMutex
	rows map[string]*NotificationPreference
	seq  int64
}

// NewInMemoryNotificationStore constructs an
// InMemoryNotificationStore.
func NewInMemoryNotificationStore() *InMemoryNotificationStore {
	return &InMemoryNotificationStore{rows: map[string]*NotificationPreference{}}
}

// List implements NotificationStore.
func (s *InMemoryNotificationStore) List(tenantID string) ([]*NotificationPreference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*NotificationPreference{}
	for _, p := range s.rows {
		if p.TenantID == tenantID {
			out = append(out, p)
		}
	}
	return out, nil
}

// ListByEvent implements NotificationStore.
func (s *InMemoryNotificationStore) ListByEvent(tenantID, eventType string) ([]*NotificationPreference, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []*NotificationPreference{}
	for _, p := range s.rows {
		if p.TenantID == tenantID && p.EventType == eventType && p.Enabled {
			out = append(out, p)
		}
	}
	return out, nil
}

// Create implements NotificationStore. It mutates p in place to
// populate the assigned ID and timestamps so the caller can return
// the row to clients.
func (s *InMemoryNotificationStore) Create(p *NotificationPreference) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	if p.ID == "" {
		p.ID = fmt.Sprintf("notif-%d", s.seq)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = time.Now().UTC()
	cp := *p
	s.rows[p.ID] = &cp
	return nil
}

// Delete implements NotificationStore.
func (s *InMemoryNotificationStore) Delete(tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok || row.TenantID != tenantID {
		return errors.New("notification: not found")
	}
	delete(s.rows, id)
	return nil
}

// NotificationDispatcher is invoked by the audit pipeline. It
// looks up subscribers and delivers the event payload.
type NotificationDispatcher struct {
	store       NotificationStore
	delivery    NotificationDelivery
	logger      func(string, ...any)
	deliveryLog NotificationDeliveryLog
}

// DeliveryResult carries per-call metadata about a notification
// delivery attempt sequence. It replaces the previous pattern of
// storing attempt/status on the WebhookDelivery struct, which
// races under concurrent Dispatch invocations (the dispatcher is
// invoked from the audit pipeline where multiple subscriptions ×
// multiple events fan out goroutines that all share one
// *WebhookDelivery instance).
type DeliveryResult struct {
	// StatusCode is the HTTP status from the final attempt (or
	// 0 when the transport itself failed). For non-webhook
	// channels this is left zero.
	StatusCode int
	// Attempts is the number of attempts the delivery consumed,
	// including the initial try. Always >= 1 when Send returned.
	Attempts int
}

// NotificationDelivery is the network port (webhook POST, email
// send). The default WebhookDelivery uses net/http. Implementations
// MUST be safe to call concurrently: per-call state lives in the
// returned DeliveryResult, never on the receiver.
type NotificationDelivery interface {
	Send(ctx context.Context, target string, channel NotificationChannel, payload []byte) (DeliveryResult, error)
}

// WebhookDelivery posts payload to target via HTTP POST.
//
// Round-7 Task 5 layers an exponential-backoff retry policy on
// top of the original net/http POST. Callers control the retry
// schedule via Backoff; on the final failure the (last) error is
// returned so the dispatcher can dead-letter the delivery and
// the notification_delivery_log records the response.
//
// WebhookDelivery is safe for concurrent use by multiple
// goroutines: all per-call state is returned as a DeliveryResult
// rather than stored on the receiver.
type WebhookDelivery struct {
	Client *http.Client
	// Backoff is the sequence of sleep durations applied between
	// retries. The slice length implicitly caps total attempts to
	// `len(Backoff)+1`. Defaults to 1s/5s/15s (3 retries +
	// initial attempt = 4 total) when nil.
	Backoff []time.Duration
	// Sleep is the test seam used between retries. Defaults to
	// time.Sleep.
	Sleep func(time.Duration)
}

// defaultWebhookBackoff matches the Round-7 spec: 1s/5s/15s.
var defaultWebhookBackoff = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}

// Send implements NotificationDelivery.
//
// On any 4xx/5xx response or transport error the delivery retries
// with exponential backoff (see Backoff). The final error
// includes the last observed status code so the dispatcher can
// dead-letter the row.
//
// Per-call state (attempts, terminal status code) is returned in
// the DeliveryResult so the receiver itself stays free of mutable
// fields and is safe for concurrent reuse across many Dispatch
// goroutines.
func (w *WebhookDelivery) Send(ctx context.Context, target string, channel NotificationChannel, payload []byte) (DeliveryResult, error) {
	var res DeliveryResult
	if channel != NotificationChannelWebhook {
		return res, errors.New("webhook delivery only supports webhook channel")
	}
	cl := w.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	backoff := w.Backoff
	if backoff == nil {
		backoff = defaultWebhookBackoff
	}
	sleep := w.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	maxAttempts := len(backoff) + 1
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res.Attempts = attempt
		res.StatusCode = 0
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return res, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			lastErr = err
		} else {
			res.StatusCode = resp.StatusCode
			_ = resp.Body.Close()
			if resp.StatusCode < 400 {
				return res, nil
			}
			lastErr = fmt.Errorf("webhook %s returned %d", target, resp.StatusCode)
			// Deterministic client errors (4xx other than 429)
			// will not recover on retry — the request body is
			// identical on every attempt, so a 400/401/403/404/
			// 422/etc. is the receiver's final answer. Returning
			// immediately avoids burning the full 1s+5s+15s
			// backoff cliff per subscriber on misconfigured
			// targets. 429 (rate limit) stays retried because the
			// receiver explicitly invites a later attempt; 5xx
			// stays retried because it's a transient server-side
			// failure.
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return res, lastErr
			}
		}
		if attempt >= maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		sleep(backoff[attempt-1])
	}
	return res, lastErr
}

// SetDeliveryLog wires the per-attempt log. Optional — when nil
// the dispatcher still delivers events but the
// /v1/admin/notifications/delivery-log endpoint will be empty.
func (d *NotificationDispatcher) SetDeliveryLog(log NotificationDeliveryLog) {
	if d != nil {
		d.deliveryLog = log
	}
}

// NewNotificationDispatcher validates and constructs the
// dispatcher.
func NewNotificationDispatcher(store NotificationStore, delivery NotificationDelivery) (*NotificationDispatcher, error) {
	if store == nil {
		return nil, errors.New("notification: nil store")
	}
	if delivery == nil {
		return nil, errors.New("notification: nil delivery")
	}
	return &NotificationDispatcher{
		store:    store,
		delivery: delivery,
		logger:   func(string, ...any) {},
	}, nil
}

// Dispatch fans the event payload out to all subscribers for
// (tenantID, eventType). Errors per delivery are logged but never
// abort the audit pipeline. When a delivery log is wired each
// attempt sequence records its terminal status.
func (d *NotificationDispatcher) Dispatch(ctx context.Context, tenantID, eventType string, payload any) error {
	subs, err := d.store.ListByEvent(tenantID, eventType)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"tenant_id": tenantID, "event_type": eventType, "payload": payload, "ts": time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	payloadMap := JSONMap{"event_type": eventType}
	if m, ok := payload.(map[string]any); ok {
		for k, v := range m {
			payloadMap[k] = v
		}
	}
	var firstErr error
	for _, p := range subs {
		result, derr := d.delivery.Send(ctx, p.Target, p.Channel, body)
		status := NotificationDeliveryStatusDelivered
		var errMsg string
		if derr != nil {
			status = NotificationDeliveryStatusFailed
			errMsg = derr.Error()
			d.logger("notification dispatch failed", "id", p.ID, "err", derr)
			if firstErr == nil {
				firstErr = derr
			}
		}
		if d.deliveryLog != nil {
			// Round-8 Task 17: when the immediate Send loop has
			// failed but the response code is retryable (i.e. not
			// a 4xx) the dispatcher schedules a follow-up attempt
			// via next_retry_at. The retry worker
			// (NotificationRetryWorker) picks these rows up and
			// re-delivers them with exponential backoff up to
			// MaxRetryAttempts.
			var nextRetry *time.Time
			if status == NotificationDeliveryStatusFailed && isRetryableResponseCode(result.StatusCode) {
				at := time.Now().UTC().Add(time.Minute)
				nextRetry = &at
			}
			_ = d.deliveryLog.Append(ctx, &NotificationDeliveryAttempt{
				TenantID:     tenantID,
				PreferenceID: p.ID,
				EventType:    eventType,
				Channel:      p.Channel,
				Target:       p.Target,
				Payload:      payloadMap,
				Status:       status,
				Attempt:      result.Attempts,
				ResponseCode: result.StatusCode,
				ErrorMessage: errMsg,
				NextRetryAt:  nextRetry,
			})
		}
	}
	return firstErr
}

// isRetryableResponseCode returns true for 5xx / transport
// failures (StatusCode == 0). 4xx are treated as permanent
// failures; the retry worker leaves them alone.
func isRetryableResponseCode(code int) bool {
	if code == 0 {
		return true
	}
	return code >= 500
}

// NotificationHandler is the admin HTTP surface.
type NotificationHandler struct {
	store NotificationStore
	audit AuditWriter
}

// NewNotificationHandler validates and constructs the handler.
func NewNotificationHandler(store NotificationStore, auditWriter AuditWriter) (*NotificationHandler, error) {
	if store == nil {
		return nil, errors.New("notification handler: nil store")
	}
	return &NotificationHandler{store: store, audit: auditWriter}, nil
}

// Register attaches routes.
func (h *NotificationHandler) Register(g *gin.RouterGroup) {
	g.GET("/v1/admin/notifications", h.list)
	g.POST("/v1/admin/notifications", h.create)
	g.DELETE("/v1/admin/notifications/:id", h.delete)
}

func (h *NotificationHandler) list(c *gin.Context) {
	tID, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tID.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	rows, err := h.store.List(tenantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"preferences": rows})
}

func (h *NotificationHandler) create(c *gin.Context) {
	tID, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tID.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	var body NotificationPreference
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body.TenantID = tenantID
	body.Enabled = true
	if err := h.store.Create(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, body)
}

func (h *NotificationHandler) delete(c *gin.Context) {
	tID, _ := c.Get(audit.TenantContextKey)
	tenantID, _ := tID.(string)
	if tenantID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if err := h.store.Delete(tenantID, id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// EmptyTestServer returns a tiny httptest server collecting
// webhook hits. Exported here so tests in other packages can wire
// it up without re-importing httptest.
func EmptyTestServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(handler)
}
