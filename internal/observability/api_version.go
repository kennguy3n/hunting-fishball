package observability

// api_version.go — Round-6 Task 13.
//
// Gin middleware that resolves the API version a request opts into
// and rejects unknown versions. Resolution order:
//
//   1. URL path prefix (`/v1/...` ⇒ "v1").
//   2. Accept-Version header.
//   3. Default = "v1".
//
// The resolved version is echoed in the X-API-Version response
// header so clients can confirm which version they actually got.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// APIVersionContextKey is the gin.Context key under which the
// resolved version is stored. Downstream handlers can read it via
// `c.GetString(observability.APIVersionContextKey)`.
const APIVersionContextKey = "api_version"

// APIVersionConfig knobs.
type APIVersionConfig struct {
	// Supported lists the versions the server accepts. The
	// default APIVersionMiddleware uses {"v1"}.
	Supported []string
	// Default is the version assumed when neither the URL prefix
	// nor the Accept-Version header is present.
	Default string
}

// APIVersionMiddleware returns a Gin middleware enforcing version
// resolution rules.
func APIVersionMiddleware(cfg APIVersionConfig) gin.HandlerFunc {
	if len(cfg.Supported) == 0 {
		cfg.Supported = []string{"v1"}
	}
	if cfg.Default == "" {
		cfg.Default = cfg.Supported[0]
	}
	supported := map[string]struct{}{}
	for _, v := range cfg.Supported {
		supported[v] = struct{}{}
	}
	return func(c *gin.Context) {
		v := resolveVersion(c.Request, cfg.Default)
		if _, ok := supported[v]; !ok {
			c.Header("X-API-Version", cfg.Default)
			c.AbortWithStatusJSON(http.StatusNotAcceptable, gin.H{
				"error":     "unsupported API version",
				"requested": v,
				"supported": cfg.Supported,
			})
			return
		}
		c.Set(APIVersionContextKey, v)
		c.Header("X-API-Version", v)
		c.Next()
	}
}

// resolveVersion picks a version from the URL prefix or the
// Accept-Version header, falling back to the default.
func resolveVersion(r *http.Request, def string) string {
	if r == nil {
		return def
	}
	path := r.URL.Path
	if len(path) >= 3 && path[0] == '/' {
		segs := strings.SplitN(path[1:], "/", 2)
		if len(segs) > 0 && isVersionToken(segs[0]) {
			return segs[0]
		}
	}
	if h := strings.TrimSpace(r.Header.Get("Accept-Version")); h != "" {
		return h
	}
	return def
}

func isVersionToken(s string) bool {
	if len(s) < 2 || (s[0] != 'v' && s[0] != 'V') {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
