//go:build e2e

// Phase 5 model-catalog e2e surface. Exercises the static model
// catalog provider through GET /v1/models/catalog, asserting the
// JSON payload contains the three Bonsai variants and the per-tier
// eviction policies the on-device runtime expects.
//
// Unlike the shard tests this file does not touch Postgres: the
// catalog is in-memory and process-global. We still tag the file
// `e2e` because the assertion exists to validate the wiring path
// the API binary uses (NewStaticCatalog → NewHandler → Register).
package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/models"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// TestSmoke_ModelCatalog asserts the static catalog response shape.
// The JSON keys are part of the contract the on-device runtime
// reads; if the server side renames a field the client falls back
// to its bundled default catalog and we lose remote control over
// model selection.
func TestSmoke_ModelCatalog(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	provider := models.NewStaticCatalog()
	h, err := models.NewHandler(provider)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	r := newTenantRouter(t, uniqueTenant(t))
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/models/catalog", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		CatalogVersion int64                  `json:"catalog_version"`
		Models         []json.RawMessage      `json:"models"`
		EvictionConfig []shard.EvictionPolicy `json:"eviction_config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.CatalogVersion < 1 {
		t.Errorf("catalog_version=%d want >= 1", resp.CatalogVersion)
	}
	if len(resp.Models) != 3 {
		t.Errorf("models=%d want 3 (q4_0/q5_0/q8_0)", len(resp.Models))
	}
	if len(resp.EvictionConfig) != 3 {
		t.Errorf("eviction_config=%d want 3 (low/mid/high)", len(resp.EvictionConfig))
	}

	// Cross-check the EligibleForTier contract through the same
	// catalog instance. Mid + High both eligible (both can run
	// the int4 build); Low ineligible because the runtime never
	// hosts the SLM.
	cat := provider.Catalog()
	if got := cat.EligibleForTier(shard.DeviceTierLow); got != nil {
		t.Errorf("Low tier should be ineligible, got %+v", got)
	}
	if got := cat.EligibleForTier(shard.DeviceTierMid); got == nil || got.Quantization != "q4_0" {
		t.Errorf("Mid tier eligible=%+v want q4_0", got)
	}
	if got := cat.EligibleForTier(shard.DeviceTierHigh); got == nil || got.Quantization != "q4_0" {
		t.Errorf("High tier eligible=%+v want q4_0", got)
	}
}
