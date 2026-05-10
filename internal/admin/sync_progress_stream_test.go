package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// fakeProgressReader returns a scripted sequence of rows. List
// returns rows[i] on call i, looping at the end. Safe for
// concurrent reads.
type fakeProgressReader struct {
	mu    sync.Mutex
	rows  [][]admin.SyncProgress
	count int
}

func (f *fakeProgressReader) List(_ context.Context, _, _ string) ([]admin.SyncProgress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.count >= len(f.rows) {
		return f.rows[len(f.rows)-1], nil
	}
	out := f.rows[f.count]
	f.count++
	return out, nil
}

func TestProgressStream_EmitsDeltasAndCompletes(t *testing.T) {
	t.Parallel()
	reader := &fakeProgressReader{rows: [][]admin.SyncProgress{
		{{NamespaceID: "ns1", Discovered: 10, Processed: 0, Failed: 0}},
		{{NamespaceID: "ns1", Discovered: 10, Processed: 5, Failed: 0}},
		{{NamespaceID: "ns1", Discovered: 10, Processed: 10, Failed: 0}},
	}}
	h := admin.NewProgressStreamHandler(reader)
	h.Poll = 10 * time.Millisecond
	h.Heartbeat = 1 * time.Hour

	g := gin.New()
	g.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	rg := g.Group("/")
	h.Register(rg)

	srv := httptest.NewServer(g)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/admin/sources/src-1/sync/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	deadline := time.Now().Add(4 * time.Second)
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if strings.Contains(string(buf), "event: completed") {
				break
			}
		}
		if err != nil {
			break
		}
	}
	body := string(buf)
	if !strings.Contains(body, "event: discovered") {
		t.Errorf("missing discovered event\n%s", body)
	}
	if !strings.Contains(body, "event: processed") {
		t.Errorf("missing processed event\n%s", body)
	}
	if !strings.Contains(body, "event: completed") {
		t.Errorf("missing completed event\n%s", body)
	}
}

func TestProgressStream_RequiresTenant(t *testing.T) {
	t.Parallel()
	h := admin.NewProgressStreamHandler(&fakeProgressReader{rows: [][]admin.SyncProgress{{}}})
	g := gin.New()
	rg := g.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src/sync/stream", nil)
	g.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
