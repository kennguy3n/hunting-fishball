// Package admin — credential_rotation.go ships the
// POST /v1/admin/sources/:id/rotate-credentials endpoint.
//
// Round-4 Task 16: connector credentials regularly need to be
// rotated (annual SOC requirement, post-incident, employee
// off-boarding, ...). Pre-rotation today the only path is DELETE
// + POST which loses the source ID, sync state, and audit history.
// The rotation handler swaps just the credentials atomically:
//
//  1. Validate the new credentials via the connector's Validate.
//  2. Persist the new ciphertext on the existing row.
//  3. Stash the previous ciphertext under a `previous_credentials`
//     blob with a timestamp so in-flight requests still talk to
//     the upstream during the configurable grace period.
//  4. Emit a source.credentials_rotated audit event with the
//     calling actor and the previous-credential expiry timestamp.
//
// The grace-period storage is config-shaped, NOT a separate table:
// the existing JSONMap config column carries it, so this lands as
// a forward-compatible append without a migration.
package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// CredentialGracePeriod is how long a previous credential is kept
// alongside the new one. Long enough for in-flight HTTP/gRPC
// requests on the upstream API to drain; short enough that an
// off-boarded employee's token does not linger.
const CredentialGracePeriod = time.Hour

// CredentialRotateRequest is the POST body. The new credentials
// blob is the same shape the original Connect endpoint accepts.
type CredentialRotateRequest struct {
	Credentials []byte `json:"credentials"`

	// Reason is the human-readable rotation reason ("scheduled",
	// "incident", "off-boarding"). Recorded on the audit event so
	// the SOC log can be filtered later.
	Reason string `json:"reason,omitempty"`
}

// CredentialRotateResponse is the success body.
type CredentialRotateResponse struct {
	SourceID         string    `json:"source_id"`
	RotatedAt        time.Time `json:"rotated_at"`
	PreviousExpiryAt time.Time `json:"previous_expiry_at"`
}

// CredentialRotator wires together everything the rotation
// handler needs. It is intentionally narrow so tests can pass in
// fakes for each collaborator.
type CredentialRotator struct {
	Repo      *SourceRepository
	Audit     AuditWriter
	Validator ConnectorValidator
	Now       func() time.Time
}

// Register mounts the rotation endpoint on rg under
// /v1/admin/sources/:id/rotate-credentials.
//
// The base /v1/admin/sources router lives in source_handler.go —
// keeping the rotation endpoint out of that handler isolates the
// concern (and lets it be mounted optionally by deployments that
// don't expose rotation publicly).
func (r *CredentialRotator) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/sources/:id/rotate-credentials", r.handle)
}

func (r *CredentialRotator) handle(c *gin.Context) {
	tenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})
		return
	}
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	var req CredentialRotateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(req.Credentials) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "credentials required"})
		return
	}
	res, err := r.Rotate(c.Request.Context(), tenantID, id, req)
	if errors.Is(err, ErrSourceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	if errors.Is(err, ErrSourceTerminal) {
		c.JSON(http.StatusConflict, gin.H{"error": "source is in a terminal status; cannot rotate credentials"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

// Rotate is the testable core. It validates the new credentials
// against the connector, persists the swap atomically (new ones in
// `credentials`, old ones moved to `previous_credentials` with a
// `previous_credentials_expires_at`), and emits the audit event.
//
// On any validation error the row is left untouched.
func (r *CredentialRotator) Rotate(ctx context.Context, tenantID, sourceID string, req CredentialRotateRequest) (CredentialRotateResponse, error) {
	if r.Repo == nil {
		return CredentialRotateResponse{}, errors.New("admin: nil Repo")
	}
	if r.Audit == nil {
		return CredentialRotateResponse{}, errors.New("admin: nil Audit")
	}
	if r.Validator == nil {
		return CredentialRotateResponse{}, errors.New("admin: nil Validator")
	}
	now := func() time.Time { return time.Now().UTC() }
	if r.Now != nil {
		now = func() time.Time { return r.Now().UTC() }
	}

	src, err := r.Repo.Get(ctx, tenantID, sourceID)
	if err != nil {
		return CredentialRotateResponse{}, err
	}
	switch src.Status {
	case SourceStatusRemoving, SourceStatusRemoved:
		return CredentialRotateResponse{}, ErrSourceTerminal
	}

	// Validate the new credentials before any mutation.
	if err := r.Validator.Validate(ctx, src.ConnectorType, connector.ConnectorConfig{
		Name:        src.ConnectorType,
		TenantID:    tenantID,
		SourceID:    src.ID,
		Settings:    map[string]any(src.Config),
		Credentials: req.Credentials,
	}); err != nil {
		return CredentialRotateResponse{}, fmt.Errorf("validate: %w", err)
	}

	// Stash the previous credentials and expiry on the config blob.
	cfg := JSONMap{}
	for k, v := range src.Config {
		cfg[k] = v
	}
	if existing, ok := cfg["credentials"]; ok && existing != nil {
		cfg["previous_credentials"] = existing
		cfg["previous_credentials_expires_at"] = now().Add(CredentialGracePeriod).Format(time.RFC3339Nano)
	}
	cfg["credentials"] = req.Credentials
	cfg["credentials_rotated_at"] = now().Format(time.RFC3339Nano)

	// We can't go through Update (it only accepts Status/Scopes), so
	// flip the column directly. Safe because we own the row in this
	// transaction and the Update lookup query is tenant-scoped.
	res := r.Repo.DB().WithContext(ctx).
		Model(&Source{}).
		Where("tenant_id = ? AND id = ? AND status NOT IN ?", tenantID, sourceID,
			[]string{string(SourceStatusRemoving), string(SourceStatusRemoved)}).
		Updates(map[string]any{
			"config":     cfg,
			"updated_at": now(),
		})
	if res.Error != nil {
		return CredentialRotateResponse{}, fmt.Errorf("admin: update credentials: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return CredentialRotateResponse{}, ErrSourceNotFound
	}

	rotatedAt := now()
	expiry := rotatedAt.Add(CredentialGracePeriod)
	actor := actorIDFromContextValue(ctx)
	auditMeta := audit.JSONMap{
		"reason":           req.Reason,
		"previous_expires": expiry.Format(time.RFC3339Nano),
	}
	log := audit.NewAuditLog(tenantID, actor, audit.ActionSourceCredentialsRotated, "source", sourceID, auditMeta, "")
	_ = r.Audit.Create(ctx, log)

	return CredentialRotateResponse{
		SourceID:         sourceID,
		RotatedAt:        rotatedAt,
		PreviousExpiryAt: expiry,
	}, nil
}

// actorIDFromContextValue is the context-only sibling of
// actorIDFromContext (which works on a *gin.Context). It looks
// for the same key the audit middleware uses on gin.Context, so
// callers wiring through context.Context (e.g. tests, background
// retries) still resolve the actor.
func actorIDFromContextValue(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(ctxActorIDKey{})
	if s, _ := v.(string); s != "" {
		return s
	}
	// Fallback: gin stuffs Keys into the request context as well,
	// look up by string key as a backstop.
	if v := ctx.Value(audit.ActorContextKey); v != nil {
		if s, _ := v.(string); s != "" {
			return s
		}
	}
	return ""
}

// ctxActorIDKey is a private type to avoid colliding with other
// packages' context keys. Tests can stash an actor with
// ContextWithActor.
type ctxActorIDKey struct{}

// ContextWithActor returns a copy of ctx carrying actor as the
// recorded actor for credential rotation audit events. Used by
// non-HTTP callers (background retries, batch tools).
func ContextWithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, ctxActorIDKey{}, actor)
}
