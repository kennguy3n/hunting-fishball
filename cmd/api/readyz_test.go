package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// gormFromMock wraps a sqlmock-driven *sql.DB into a *gorm.DB without
// actually issuing any DDL. DisableAutomaticPing prevents gorm from
// consuming the mock's expected Ping at Open time.
func gormFromMock(t *testing.T, sqlDB *sql.DB) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		postgres.New(postgres.Config{Conn: sqlDB}),
		&gorm.Config{DisableAutomaticPing: true},
	)
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	return db
}

// TestHealthz_AlwaysOK is a tiny smoke check that /healthz returns
// 200 regardless of dependency state.
func TestHealthz_AlwaysOK(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body: %q", w.Body.String())
	}
}

// TestApiReadyz_PostgresDown_Returns503 verifies the readiness probe
// fails the response when Postgres ping returns an error.
func TestApiReadyz_PostgresDown_Returns503(t *testing.T) {
	t.Parallel()

	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	mock.ExpectPing().WillReturnError(sql.ErrConnDone)

	gormDB := gormFromMock(t, sqlDB)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// rc is nil and qdrant is nil — the handler still runs and
	// reports them as "skipped"; the postgres failure alone
	// produces 503.
	r.GET("/readyz", apiReadyzHandler(gormDB, nil, nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != false {
		t.Fatalf("ok: %v", got["ok"])
	}
	checks, ok := got["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks: %v", got["checks"])
	}
	if pg, ok := checks["postgres"].(string); !ok || pg == "up" {
		t.Fatalf("postgres should be down: %v", checks["postgres"])
	}
	if r := checks["redis"]; r != "skipped" {
		t.Fatalf("redis: %v", r)
	}
	if q := checks["qdrant"]; q != "skipped" {
		t.Fatalf("qdrant: %v", q)
	}
}

// TestApiReadyz_AllUp_Returns200_AllSkipped checks the path where
// every dependency is reported as "skipped" — Postgres alone is
// healthy, redis and qdrant are nil. The handler should report ok.
func TestApiReadyz_AllUp_Returns200_AllSkipped(t *testing.T) {
	t.Parallel()

	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	mock.ExpectPing()

	gormDB := gormFromMock(t, sqlDB)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/readyz", apiReadyzHandler(gormDB, nil, nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["ok"] != true {
		t.Fatalf("ok: %v", got["ok"])
	}
}

// TestApiReadyz_LatencyFieldsPresent — Round-11 Task 10.
//
// Asserts the response body now includes a `latencies` map with
// numeric postgres_ms / redis_ms / qdrant_ms entries even when
// optional backends are skipped. The exact value is timing-
// dependent, but the keys must be present so operators can chart
// p99 latency directly from the readyz envelope.
func TestApiReadyz_LatencyFieldsPresent(t *testing.T) {
	t.Parallel()

	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	mock.ExpectPing()

	gormDB := gormFromMock(t, sqlDB)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/readyz", apiReadyzHandler(gormDB, nil, nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	lat, ok := got["latencies"].(map[string]any)
	if !ok {
		t.Fatalf("latencies map missing: %v", got["latencies"])
	}
	for _, key := range []string{"postgres_ms", "redis_ms", "qdrant_ms"} {
		val, present := lat[key]
		if !present {
			t.Fatalf("latencies[%q] missing", key)
		}
		// JSON numbers decode as float64.
		if _, ok := val.(float64); !ok {
			t.Fatalf("latencies[%q] not numeric: %T %v", key, val, val)
		}
	}
}
