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
	store    NotificationStore
	delivery NotificationDelivery
	logger   func(string, ...any)
}

// NotificationDelivery is the network port (webhook POST, email
// send). The default WebhookDelivery uses net/http.
type NotificationDelivery interface {
	Send(ctx context.Context, target string, channel NotificationChannel, payload []byte) error
}

// WebhookDelivery posts payload to target via HTTP POST.
type WebhookDelivery struct {
	Client *http.Client
}

// Send implements NotificationDelivery.
func (w *WebhookDelivery) Send(ctx context.Context, target string, channel NotificationChannel, payload []byte) error {
	if channel != NotificationChannelWebhook {
		return errors.New("webhook delivery only supports webhook channel")
	}
	cl := w.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook %s returned %d", target, resp.StatusCode)
	}
	return nil
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
// abort the audit pipeline.
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
	var firstErr error
	for _, p := range subs {
		if err := d.delivery.Send(ctx, p.Target, p.Channel, body); err != nil {
			d.logger("notification dispatch failed", "id", p.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
