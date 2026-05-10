package models_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/models"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

func TestStaticCatalog_Validate(t *testing.T) {
	t.Parallel()
	cat := models.NewStaticCatalog().Catalog()
	if err := cat.Validate(); err != nil {
		t.Fatalf("static catalog validate: %v", err)
	}
	if cat.CatalogVersion <= 0 {
		t.Fatalf("static version: %d", cat.CatalogVersion)
	}
	if len(cat.Models) == 0 {
		t.Fatalf("expected non-empty static catalog")
	}
	if len(cat.EvictionConfig) != 3 {
		t.Fatalf("eviction config: %+v", cat.EvictionConfig)
	}
}

func TestModelCatalog_Validate_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cat  models.ModelCatalog
	}{
		{
			name: "missing version",
			cat: models.ModelCatalog{
				Models: []models.ModelEntry{{ID: "x", Family: "f", TierFloor: shard.DeviceTierLow, SizeMB: 1}},
			},
		},
		{
			name: "missing entry id",
			cat: models.ModelCatalog{
				CatalogVersion: 1,
				Models:         []models.ModelEntry{{Family: "f", TierFloor: shard.DeviceTierLow, SizeMB: 1}},
			},
		},
		{
			name: "missing tier",
			cat: models.ModelCatalog{
				CatalogVersion: 1,
				Models:         []models.ModelEntry{{ID: "x", Family: "f", SizeMB: 1}},
			},
		},
		{
			name: "non-positive size",
			cat: models.ModelCatalog{
				CatalogVersion: 1,
				Models:         []models.ModelEntry{{ID: "x", Family: "f", TierFloor: shard.DeviceTierLow, SizeMB: 0}},
			},
		},
		{
			name: "duplicate id",
			cat: models.ModelCatalog{
				CatalogVersion: 1,
				Models: []models.ModelEntry{
					{ID: "x", Family: "f", TierFloor: shard.DeviceTierLow, SizeMB: 1},
					{ID: "x", Family: "f", TierFloor: shard.DeviceTierMid, SizeMB: 2},
				},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.cat.Validate(); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestModelCatalog_EligibleForTier(t *testing.T) {
	t.Parallel()
	cat := models.NewStaticCatalog().Catalog()

	if got := cat.EligibleForTier(shard.DeviceTierLow); got != nil {
		t.Fatalf("low tier should not have an entry, got %+v", got)
	}
	mid := cat.EligibleForTier(shard.DeviceTierMid)
	if mid == nil || mid.ID != "bonsai-1.7b-q4_0" {
		t.Fatalf("mid tier eligible: %+v", mid)
	}
	high := cat.EligibleForTier(shard.DeviceTierHigh)
	// Two entries are eligible at High; the smallest (q8_0,
	// 1800 MB) should win over fp16 (3400 MB).
	if high == nil || high.ID != "bonsai-1.7b-q8_0" {
		t.Fatalf("high tier eligible: %+v", high)
	}
	if got := cat.EligibleForTier(shard.DeviceTierUnknown); got != nil {
		t.Fatalf("unknown tier eligible: %+v", got)
	}
}

func TestStaticProvider_Catalog_DefensiveCopy(t *testing.T) {
	t.Parallel()
	p := models.NewStaticCatalog()
	cat := p.Catalog()
	cat.Models[0].ID = "tampered"
	cat.EvictionConfig[0].MaxShardSizeMB = 9999

	again := p.Catalog()
	if again.Models[0].ID == "tampered" {
		t.Fatalf("StaticProvider returned aliased Models slice")
	}
	if again.EvictionConfig[0].MaxShardSizeMB == 9999 {
		t.Fatalf("StaticProvider returned aliased EvictionConfig slice")
	}
}

func TestHandler_Catalog(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h, err := models.NewHandler(models.NewStaticCatalog())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	rg := r.Group("")
	h.Register(rg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models/catalog", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got models.ModelCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CatalogVersion <= 0 {
		t.Fatalf("version: %d", got.CatalogVersion)
	}
	if len(got.Models) == 0 {
		t.Fatalf("models empty")
	}
	if len(got.EvictionConfig) != 3 {
		t.Fatalf("eviction_config len=%d", len(got.EvictionConfig))
	}
}

func TestHandler_NewHandler_NilProvider(t *testing.T) {
	t.Parallel()
	if _, err := models.NewHandler(nil); err == nil {
		t.Fatalf("expected error for nil provider")
	}
}
