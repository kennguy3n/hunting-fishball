package main

import (
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/observability"
)

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

func TestIngestHealthz_AlwaysOK(t *testing.T) {
	t.Parallel()

	sqlDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	gormDB := gormFromMock(t, sqlDB)

	mux := ingestHTTPHandler(gormDB, nil, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Fatalf("body: %q", w.Body.String())
	}
}

// TestIngestReadyz_BrokerUnreachable_Returns503 spins up a TCP
// listener that we close before the probe runs, so the broker dial
// attempt fails and the handler returns 503.
func TestIngestReadyz_BrokerUnreachable_Returns503(t *testing.T) {
	t.Parallel()

	// Reserve a TCP port and close the listener so dialling
	// produces an immediate "connection refused".
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()

	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	mock.ExpectPing()
	gormDB := gormFromMock(t, sqlDB)

	mux := ingestHTTPHandler(gormDB, nil, []string{addr})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	mux.ServeHTTP(w, req)

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
	kafka, _ := checks["kafka"].(string)
	if !strings.HasPrefix(kafka, "down:") {
		t.Fatalf("kafka should be down: %v", checks["kafka"])
	}
}

// TestIngestReadyz_AllUp_Returns200 spins up a real TCP listener so
// the broker dial succeeds; with Postgres healthy and no Redis
// configured the probe returns 200.
func TestIngestReadyz_AllUp_Returns200(t *testing.T) {
	t.Parallel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = lis.Close() }()
	addr := lis.Addr().String()

	sqlDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	mock.ExpectPing()
	gormDB := gormFromMock(t, sqlDB)

	mux := ingestHTTPHandler(gormDB, nil, []string{addr})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/readyz", nil)
	mux.ServeHTTP(w, req)

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

func TestIngestMetrics_EndpointResponds(t *testing.T) {
	t.Parallel()

	sqlDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	gormDB := gormFromMock(t, sqlDB)

	// Record one Kafka lag sample so the collector has at least one
	// labelled series to render — Vec metrics never appear in
	// /metrics output until they've been observed at least once.
	observability.SetKafkaConsumerLag("topic-x", "0", "test-group", 0)

	mux := ingestHTTPHandler(gormDB, nil, nil)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/metrics", nil)
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "context_engine_kafka_consumer_lag") {
		t.Fatalf("metrics body missing context_engine_ collectors: %s", w.Body.String())
	}
}
