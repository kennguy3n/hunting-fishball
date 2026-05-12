// onboarding_handler.go — Round-18 Task 13.
//
// Tenant onboarding wizard API. The admin portal calls
// POST /v1/admin/tenants/:tenant_id/onboarding to determine whether
// a freshly-created tenant has completed the minimum-viable setup
// to start ingesting and querying:
//
//   - At least one source connected (otherwise nothing to retrieve).
//   - At least one policy draft promoted (otherwise PII / channel
//     gating is wide-open).
//   - A health check on the engine (so we don't tell an operator
//     "you're good" when Kafka is melting).
//
// The handler is purely a validator — it doesn't mutate tenant
// state. The response is structured so the portal can render a
// per-check status and a follow-up CTA. Mirrors the contract used
// by the existing admin health summary handler.

package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// OnboardingSourceCounter is the narrow contract the wizard needs
// from the source repository. *SourceRepository satisfies it via
// ListAllForTenant.
type OnboardingSourceCounter interface {
	ListAllForTenant(ctx context.Context, tenantID string) ([]Source, error)
}

// OnboardingPolicyCounter is the narrow contract the wizard needs
// from policy storage. Returns the number of promoted policy
// versions for the tenant (anything > 0 satisfies the gate).
type OnboardingPolicyCounter interface {
	PromotedPolicyVersionCount(ctx context.Context, tenantID string) (int, error)
}

// OnboardingHealthChecker is the narrow contract the wizard uses
// to verify the engine is reachable. Returns nil for healthy or
// an error explaining why.
type OnboardingHealthChecker interface {
	CheckHealth(ctx context.Context) error
}

// OnboardingHandlerConfig configures the wizard.
type OnboardingHandlerConfig struct {
	Sources  OnboardingSourceCounter
	Policies OnboardingPolicyCounter
	Health   OnboardingHealthChecker
}

// OnboardingHandler serves POST /v1/admin/tenants/:tenant_id/onboarding.
type OnboardingHandler struct {
	cfg OnboardingHandlerConfig
}

// NewOnboardingHandler validates cfg and returns the handler.
func NewOnboardingHandler(cfg OnboardingHandlerConfig) (*OnboardingHandler, error) {
	if cfg.Sources == nil {
		return nil, errors.New("admin onboarding: nil Sources")
	}
	if cfg.Policies == nil {
		return nil, errors.New("admin onboarding: nil Policies")
	}
	if cfg.Health == nil {
		return nil, errors.New("admin onboarding: nil Health")
	}

	return &OnboardingHandler{cfg: cfg}, nil
}

// Register mounts the route.
func (h *OnboardingHandler) Register(rg *gin.RouterGroup) {
	rg.POST("/v1/admin/tenants/:tenant_id/onboarding", h.post)
}

// OnboardingCheck represents one prerequisite gate.
type OnboardingCheck struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Detail  string `json:"detail,omitempty"`
	NextURL string `json:"next_url,omitempty"`
}

// OnboardingResponse is the JSON shape returned to callers.
type OnboardingResponse struct {
	TenantID string            `json:"tenant_id"`
	Ready    bool              `json:"ready"`
	Checks   []OnboardingCheck `json:"checks"`
}

func (h *OnboardingHandler) post(c *gin.Context) {
	ctxTenantID, ok := tenantIDFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing tenant context"})

		return
	}
	pathTenantID := c.Param("tenant_id")
	if pathTenantID == "" || pathTenantID != ctxTenantID {
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant context mismatch"})

		return
	}
	ctx := c.Request.Context()
	resp := OnboardingResponse{TenantID: ctxTenantID, Checks: []OnboardingCheck{}}

	srcCheck := OnboardingCheck{Name: "sources_connected", NextURL: "/v1/admin/sources"}
	srcs, err := h.cfg.Sources.ListAllForTenant(ctx, ctxTenantID)
	if err != nil {
		srcCheck.Detail = "could not list sources: " + err.Error()
	} else if len(srcs) == 0 {
		srcCheck.Detail = "no sources connected; connect at least one to start ingesting"
	} else {
		srcCheck.Passed = true
	}
	resp.Checks = append(resp.Checks, srcCheck)

	polCheck := OnboardingCheck{Name: "policy_promoted", NextURL: "/v1/admin/policies"}
	n, err := h.cfg.Policies.PromotedPolicyVersionCount(ctx, ctxTenantID)
	if err != nil {
		polCheck.Detail = "could not count promoted policies: " + err.Error()
	} else if n == 0 {
		polCheck.Detail = "no policy promoted; promote at least one policy version to gate access"
	} else {
		polCheck.Passed = true
	}
	resp.Checks = append(resp.Checks, polCheck)

	healthCheck := OnboardingCheck{Name: "engine_health"}
	if err := h.cfg.Health.CheckHealth(ctx); err != nil {
		healthCheck.Detail = "engine unhealthy: " + err.Error()
	} else {
		healthCheck.Passed = true
	}
	resp.Checks = append(resp.Checks, healthCheck)

	resp.Ready = true
	for _, ck := range resp.Checks {
		if !ck.Passed {
			resp.Ready = false

			break
		}
	}

	c.JSON(http.StatusOK, resp)
}
