package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func TestConnectorTemplate_Validation(t *testing.T) {
	cases := []*admin.ConnectorTemplate{
		nil,
		{ConnectorType: "google-drive"},
		{TenantID: "t"},
	}
	for i, tc := range cases {
		if err := tc.Validate(); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
	good := &admin.ConnectorTemplate{TenantID: "t", ConnectorType: "google-drive"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good case failed: %v", err)
	}
}

func TestConnectorTemplate_CRUD(t *testing.T) {
	s := admin.NewInMemoryConnectorTemplateStore()
	tmpl := &admin.ConnectorTemplate{
		TenantID: "t", ConnectorType: "google-drive",
		DefaultConfig: admin.JSONMap{"page_size": 100},
	}
	if err := s.Create(tmpl); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tmpl.ID == "" {
		t.Fatalf("ID not assigned")
	}
	rows, _ := s.List("t")
	if len(rows) != 1 {
		t.Fatalf("list len=%d", len(rows))
	}
	got, err := s.Get("t", tmpl.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if err := s.Delete("t", tmpl.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestConnectorTemplate_TenantIsolation(t *testing.T) {
	s := admin.NewInMemoryConnectorTemplateStore()
	_ = s.Create(&admin.ConnectorTemplate{TenantID: "ta", ConnectorType: "x"})
	rows, _ := s.List("tb")
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %v", rows)
	}
}

func TestApplyTemplate_RequestOverridesTemplate(t *testing.T) {
	tmpl := &admin.ConnectorTemplate{
		DefaultConfig: admin.JSONMap{"page_size": 100, "drive_id": "abc"},
	}
	req := admin.JSONMap{"drive_id": "override", "extra": true}
	merged := admin.ApplyTemplate(tmpl, req)
	if merged["page_size"].(int) != 100 {
		t.Fatalf("template default lost: %+v", merged)
	}
	if merged["drive_id"].(string) != "override" {
		t.Fatalf("request override lost: %+v", merged)
	}
	if merged["extra"].(bool) != true {
		t.Fatalf("extra request key lost: %+v", merged)
	}
}

func TestApplyTemplate_NilTemplate(t *testing.T) {
	merged := admin.ApplyTemplate(nil, admin.JSONMap{"x": 1})
	if merged["x"].(int) != 1 {
		t.Fatalf("nil template should still apply request: %+v", merged)
	}
}

func TestConnectorTemplateHandler_HTTP(t *testing.T) {
	store := admin.NewInMemoryConnectorTemplateStore()
	h, err := admin.NewConnectorTemplateHandler(store, &fakeAudit{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body, _ := json.Marshal(admin.ConnectorTemplate{
		ConnectorType: "google-drive",
		DefaultConfig: admin.JSONMap{"page_size": 100},
		Description:   "Default Drive template",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/connector-templates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body)
	}
	var created admin.ConnectorTemplate
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("ID not returned")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/connector-templates", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
}
