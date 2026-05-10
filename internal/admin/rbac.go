// Package admin — rbac.go ships the role-based access middleware
// that gates the /v1/admin/* surface.
//
// Round-5 Task 4: every admin endpoint is now mounted behind a
// middleware that requires the request's role to satisfy the
// route's policy. Two production roles are defined up front
// (admin, viewer); a third role (super_admin) is reserved for the
// cross-tenant analytics endpoint introduced in Task 19. The
// middleware reads the role from the request context (typed key
// or the gin.Context value) — production wiring populates it from
// the OIDC ID token claims, the dev fallback reads the
// `X-Role` header so local tests don't need a real IdP.
package admin

import (
	stderrors "errors"

	"github.com/gin-gonic/gin"

	apperrors "github.com/kennguy3n/hunting-fishball/internal/errors"
)

// Role is the set of permission classes the RBAC middleware
// recognises. Compare with === — additional roles must be added
// to RoleAllowed and the inclusion graph below.
type Role string

const (
	// RoleAdmin grants full CRUD on the admin surface.
	RoleAdmin Role = "admin"
	// RoleViewer grants GET-only access (HEAD / OPTIONS are
	// implicit).
	RoleViewer Role = "viewer"
	// RoleSuperAdmin is the cross-tenant role used by the global
	// analytics endpoint. It satisfies every per-tenant policy a
	// regular admin satisfies.
	RoleSuperAdmin Role = "super_admin"
	// RoleAnonymous is the explicit "no role" sentinel — handlers
	// that surface this in errors must NEVER imply that an
	// unauthenticated caller had a partial role.
	RoleAnonymous Role = ""
)

// RoleContextKey is the Gin context key the auth middleware writes
// the resolved Role under. Mirrors audit.TenantContextKey.
const RoleContextKey = "role"

// RoleHeader is the dev fallback header. Production deployments
// must NOT trust it — auth middleware strips the header from
// inbound requests before this middleware runs and only ever
// writes the OIDC-derived role to the context. The header is
// useful for local httptest tables and the docker-compose smoke
// suite.
const RoleHeader = "X-Role"

// roleSatisfies returns true iff `have` grants `need`. The
// inclusion graph: super_admin > admin > viewer.
func roleSatisfies(have, need Role) bool {
	if have == need {
		return true
	}
	switch need {
	case RoleViewer:
		return have == RoleAdmin || have == RoleSuperAdmin
	case RoleAdmin:
		return have == RoleSuperAdmin
	case RoleSuperAdmin:
		return false
	}
	return false
}

// RoleAllowed reports whether r is one of the documented roles
// (excluding the anonymous sentinel). Used by tests / handlers
// that need to validate a role string before persisting it.
func RoleAllowed(r Role) bool {
	switch r {
	case RoleAdmin, RoleViewer, RoleSuperAdmin:
		return true
	}
	return false
}

// RoleFromContext returns the role on the Gin context. The
// (Role, true) shape mirrors tenantIDFromContext.
func RoleFromContext(c *gin.Context) (Role, bool) {
	v, ok := c.Get(RoleContextKey)
	if !ok {
		// Fall back to the dev header so unit tests can drive the
		// middleware without an OIDC stand-in. Production auth
		// middleware strips the header before the request runs
		// through this stack.
		if h := c.GetHeader(RoleHeader); h != "" {
			return Role(h), true
		}
		return RoleAnonymous, false
	}
	switch v := v.(type) {
	case Role:
		return v, true
	case string:
		return Role(v), true
	}
	return RoleAnonymous, false
}

// RequireRole returns a Gin middleware that aborts unless the
// caller's role satisfies `need`.
//
// Errors are emitted via the structured error catalogue
// (internal/errors) so the response body matches the rest of the
// admin API. The middleware never logs the requested role — that
// is the auth middleware's responsibility.
func RequireRole(need Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		have, ok := RoleFromContext(c)
		if !ok || have == RoleAnonymous {
			_ = c.Error(apperrors.Wrap(apperrors.CodeUnauthenticated,
				stderrors.New("admin: missing role on request")))
			c.AbortWithStatus(401)
			return
		}
		if !roleSatisfies(have, need) {
			_ = c.Error(apperrors.New(apperrors.CodeForbidden,
				"admin: role does not satisfy required permission").
				With("required_role", string(need)).
				With("actor_role", string(have)))
			c.AbortWithStatus(403)
			return
		}
		c.Next()
	}
}

// MethodRBAC returns a middleware that gates by HTTP method:
// safe methods (GET / HEAD / OPTIONS) require `read`; mutating
// methods require `write`. The two roles `read` and `write`
// resolve into the standard role hierarchy via roleSatisfies.
//
// This is the recommended way to mount RBAC across an entire
// route group when the GET-vs-mutate split is the only axis a
// caller cares about — e.g. /v1/admin/sources where every GET is
// safe for viewers and every POST/PATCH/DELETE requires admin.
func MethodRBAC() gin.HandlerFunc {
	return func(c *gin.Context) {
		need := RoleViewer
		switch c.Request.Method {
		case "POST", "PUT", "PATCH", "DELETE":
			need = RoleAdmin
		}
		// Re-use RequireRole's logic without double-binding the
		// middleware to the route — call the inner closure
		// directly so the abort statuses stay consistent.
		RequireRole(need)(c)
	}
}
