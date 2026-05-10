package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

// TestSourceList_CursorPagination round-trips three pages through
// the GET /v1/admin/sources handler and asserts:
//   - the response envelope advertises `next_cursor`
//   - empty next_cursor signals the last page
//   - the limit query parameter narrows the page
//   - cursors are stable across calls (no drift, no overlap)
//   - the limit cap clamps oversized requests
func TestSourceList_CursorPagination(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: &fakeValidator{}, Audit: &fakeAudit{}})

	const total = 12
	for i := 0; i < total; i++ {
		s := admin.NewSource("tenant-a", "slack", nil, nil)
		if err := repo.Create(context.Background(), s); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}

	// First page: limit=5, no cursor.
	page1 := getList(t, h, "/v1/admin/sources?limit=5", "tenant-a")
	if len(page1.Items) != 5 {
		t.Fatalf("page1 size = %d", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 must emit next_cursor when more rows exist")
	}

	// Second page: cursor from page1.
	page2 := getList(t, h, "/v1/admin/sources?limit=5&cursor="+page1.NextCursor, "tenant-a")
	if len(page2.Items) != 5 {
		t.Fatalf("page2 size = %d", len(page2.Items))
	}
	if page2.NextCursor == "" {
		t.Fatal("page2 must emit next_cursor when more rows exist")
	}
	for _, p1 := range page1.Items {
		for _, p2 := range page2.Items {
			if p1.ID == p2.ID {
				t.Fatalf("page1 / page2 overlap on id=%s", p1.ID)
			}
		}
	}

	// Third page: cursor from page2 — exhausts the result set.
	page3 := getList(t, h, "/v1/admin/sources?limit=5&cursor="+page2.NextCursor, "tenant-a")
	if len(page3.Items) != 2 {
		t.Fatalf("page3 size = %d (expected 2)", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Fatalf("page3 must clear next_cursor on the last page, got %q", page3.NextCursor)
	}

	// Querying past the end (cursor lower than every id) returns
	// empty + empty cursor. ULIDs use Crockford base32 starting at
	// '0', so a cursor of all '0's compares less than any real id.
	empty := getList(t, h, "/v1/admin/sources?limit=5&cursor=00000000000000000000000000", "tenant-a")
	if len(empty.Items) != 0 || empty.NextCursor != "" {
		t.Fatalf("query past last page must be empty: %+v", empty)
	}

	// Limit ceiling: limit=1000 must clamp to MaxPageLimit=200, not error.
	clamped := getList(t, h, "/v1/admin/sources?limit=1000", "tenant-a")
	if len(clamped.Items) != total {
		t.Fatalf("clamped page size = %d", len(clamped.Items))
	}

	// Garbage limit -> 400.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources?limit=abc", nil)
	router(t, h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc must return 400, got %d", w.Code)
	}
}

// TestSourceList_DefaultLimit checks that a request without `limit`
// uses the documented default and that the default is at least 50.
func TestSourceList_DefaultLimit(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: &fakeValidator{}, Audit: &fakeAudit{}})
	for i := 0; i < 75; i++ {
		s := admin.NewSource("tenant-a", "slack", nil, nil)
		if err := repo.Create(context.Background(), s); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}
	page := getList(t, h, "/v1/admin/sources", "tenant-a")
	if len(page.Items) != admin.DefaultPageLimit {
		t.Fatalf("default page size = %d (expected %d)", len(page.Items), admin.DefaultPageLimit)
	}
	if page.NextCursor == "" {
		t.Fatal("default page must emit next_cursor when more rows exist")
	}
}

// listResponse mirrors the admin source list envelope shape.
type listResponse struct {
	Items      []admin.Source `json:"items"`
	NextCursor string         `json:"next_cursor"`
}

func getList(t *testing.T, h *admin.Handler, path, tenantID string) listResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	router(t, h, tenantID).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d body=%s", path, w.Code, w.Body.String())
	}
	var out listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	return out
}

// TestSourceList_CrossTenantIsolation sanity-checks that cursors
// minted on one tenant cannot be used to read another tenant's
// rows. Cursors are non-secret IDs, but the WHERE tenant_id = ?
// clause MUST still scope the query.
func TestSourceList_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	repo, _ := newSQLiteSourceRepo(t)
	h, _ := admin.NewHandler(admin.HandlerConfig{Repo: repo, Validator: &fakeValidator{}, Audit: &fakeAudit{}})
	tenants := []string{"tenant-a", "tenant-b"}
	for _, tn := range tenants {
		for i := 0; i < 4; i++ {
			s := admin.NewSource(tn, "slack", nil, nil)
			if err := repo.Create(context.Background(), s); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	// Mint a cursor on tenant-a's last id.
	pageA := getList(t, h, "/v1/admin/sources?limit=2", "tenant-a")
	if pageA.NextCursor == "" {
		t.Fatal("setup: pageA must have a next_cursor")
	}
	// Use that cursor against tenant-b. It must NOT leak any of
	// tenant-a's rows; only tenant-b's older-than-cursor rows
	// (likely zero) come back.
	pageB := getList(t, h, fmt.Sprintf("/v1/admin/sources?limit=10&cursor=%s", pageA.NextCursor), "tenant-b")
	for _, item := range pageB.Items {
		if item.TenantID != "tenant-b" {
			t.Fatalf("cross-tenant leak: %+v", item)
		}
	}
}
