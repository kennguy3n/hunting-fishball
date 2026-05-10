package admin_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	apperrors "github.com/kennguy3n/hunting-fishball/internal/errors"
)

// rbacRouter wires RoleContextKey from a constant role and mounts
// the supplied middleware in front of a stub handler. Tests
// observe the route's status / body to assert allow vs deny.
func rbacRouter(t *testing.T, role admin.Role, mw gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(apperrors.Middleware())
	if role != admin.RoleAnonymous {
		r.Use(func(c *gin.Context) {
			c.Set(admin.RoleContextKey, role)
			c.Next()
		})
	}
	g := r.Group("/v1/admin")
	g.Use(mw)
	g.GET("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	g.POST("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	g.DELETE("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r
}

func TestRBAC_RoleSatisfies_Inclusion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		role admin.Role
		mw   gin.HandlerFunc
		path string
		verb string
		want int
	}{
		// admin satisfies viewer
		{"admin/GET via RequireRole(viewer)", admin.RoleAdmin, admin.RequireRole(admin.RoleViewer), "/v1/admin/ping", http.MethodGet, http.StatusOK},
		// viewer cannot satisfy admin
		{"viewer/POST via RequireRole(admin)", admin.RoleViewer, admin.RequireRole(admin.RoleAdmin), "/v1/admin/ping", http.MethodPost, http.StatusForbidden},
		// super_admin satisfies admin
		{"super_admin/DELETE via RequireRole(admin)", admin.RoleSuperAdmin, admin.RequireRole(admin.RoleAdmin), "/v1/admin/ping", http.MethodDelete, http.StatusOK},
		// super_admin satisfies super_admin
		{"super_admin/GET via RequireRole(super_admin)", admin.RoleSuperAdmin, admin.RequireRole(admin.RoleSuperAdmin), "/v1/admin/ping", http.MethodGet, http.StatusOK},
		// admin does NOT satisfy super_admin
		{"admin/GET via RequireRole(super_admin)", admin.RoleAdmin, admin.RequireRole(admin.RoleSuperAdmin), "/v1/admin/ping", http.MethodGet, http.StatusForbidden},
		// no role -> 401
		{"anonymous/GET", admin.RoleAnonymous, admin.RequireRole(admin.RoleViewer), "/v1/admin/ping", http.MethodGet, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := rbacRouter(t, tc.role, tc.mw)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.verb, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("status: got %d want %d body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestRBAC_MethodRBAC_GETPOSTDELETE asserts the method-based
// middleware: safe methods only require viewer, mutating methods
// require admin.
func TestRBAC_MethodRBAC_GETPOSTDELETE(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role admin.Role
		verb string
		want int
	}{
		// viewer can GET but not mutate.
		{admin.RoleViewer, http.MethodGet, http.StatusOK},
		{admin.RoleViewer, http.MethodPost, http.StatusForbidden},
		{admin.RoleViewer, http.MethodDelete, http.StatusForbidden},
		// admin satisfies both.
		{admin.RoleAdmin, http.MethodGet, http.StatusOK},
		{admin.RoleAdmin, http.MethodPost, http.StatusOK},
		{admin.RoleAdmin, http.MethodDelete, http.StatusOK},
		// super_admin satisfies both.
		{admin.RoleSuperAdmin, http.MethodGet, http.StatusOK},
		{admin.RoleSuperAdmin, http.MethodPost, http.StatusOK},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.role)+"/"+tc.verb, func(t *testing.T) {
			t.Parallel()
			r := rbacRouter(t, tc.role, admin.MethodRBAC())
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.verb, "/v1/admin/ping", nil))
			if w.Code != tc.want {
				t.Fatalf("status: got %d want %d body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestRBAC_HeaderFallback verifies the dev fallback: when no role
// is set on the context, the middleware reads X-Role. Production
// auth middleware MUST strip this header before reaching here, so
// this is purely a local-dev / unit-test affordance.
func TestRBAC_HeaderFallback(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(apperrors.Middleware())
	g := r.Group("/v1/admin")
	g.Use(admin.RequireRole(admin.RoleAdmin))
	g.GET("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	// Header carrying admin -> 200
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/ping", nil)
	req.Header.Set(admin.RoleHeader, string(admin.RoleAdmin))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("X-Role admin: got %d body=%s", w.Code, w.Body.String())
	}

	// Header carrying viewer against admin-required route -> 403
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/ping", nil)
	req.Header.Set(admin.RoleHeader, string(admin.RoleViewer))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("X-Role viewer vs admin route: got %d body=%s", w.Code, w.Body.String())
	}
}

// TestRBAC_ForbiddenBodyShape checks that 403 responses go
// through the structured error catalogue (matching the rest of
// the admin API).
func TestRBAC_ForbiddenBodyShape(t *testing.T) {
	t.Parallel()
	r := rbacRouter(t, admin.RoleViewer, admin.RequireRole(admin.RoleAdmin))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/ping", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: %d", w.Code)
	}
	if !contains(w.Body.String(), "ERR_FORBIDDEN") {
		t.Fatalf("body must reference ERR_FORBIDDEN: %s", w.Body.String())
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestRBAC_RoleAllowed sanity-checks the public Role validator.
func TestRBAC_RoleAllowed(t *testing.T) {
	t.Parallel()
	for _, r := range []admin.Role{admin.RoleAdmin, admin.RoleViewer, admin.RoleSuperAdmin} {
		if !admin.RoleAllowed(r) {
			t.Fatalf("RoleAllowed(%q) = false", r)
		}
	}
	for _, r := range []admin.Role{"", "root", "guest"} {
		if admin.RoleAllowed(r) {
			t.Fatalf("RoleAllowed(%q) = true", r)
		}
	}
}
