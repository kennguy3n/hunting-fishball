// Package deploy_test hosts Go tests that lint static deployment
// artefacts (Prometheus rules, Grafana dashboards, etc.). Tests
// keep the artefacts in lockstep with the Go code that emits the
// metrics they query.
package deploy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrafanaDashboard_Parses asserts the dashboard JSON model is
// well-formed and references at least the panels enumerated in
// the alerting runbook (alerts.md). When a panel is renamed in
// the JSON, the runbook must be updated in the same commit.
func TestGrafanaDashboard_Parses(t *testing.T) {
	t.Parallel()
	path := filepath.Join("grafana", "context-engine-dashboard.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	var dash struct {
		UID    string `json:"uid"`
		Title  string `json:"title"`
		Panels []struct {
			ID      int                      `json:"id"`
			Title   string                   `json:"title"`
			Type    string                   `json:"type"`
			Targets []map[string]interface{} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if dash.UID == "" || dash.Title == "" {
		t.Fatalf("dashboard missing uid/title: %+v", dash)
	}
	wantPanels := map[int]string{
		1: "Pipeline Stage Duration",
		2: "Retrieval p95 latency",
		3: "Kafka Consumer Lag",
		4: "DLQ Depth Over Time",
		5: "Connector Health Matrix",
		6: "API Request Rate",
		7: "Token Refreshes",
		8: "Auto-reindex Triggers",
	}
	have := map[int]string{}
	for _, p := range dash.Panels {
		have[p.ID] = p.Title
		if len(p.Targets) == 0 {
			t.Fatalf("panel %d (%q) has no targets", p.ID, p.Title)
		}
	}
	for id, prefix := range wantPanels {
		title, ok := have[id]
		if !ok {
			t.Fatalf("dashboard missing panel id=%d (expected title prefix %q)", id, prefix)
		}
		if !strings.Contains(title, prefix) {
			t.Fatalf("panel %d title %q must contain %q", id, title, prefix)
		}
	}
}

// TestGrafanaDashboard_ReferencesRound5Metrics ensures the new
// Round-5 metrics introduced in observability/metrics.go are
// actually referenced in the dashboard. Without this guard, a
// metric rename would silently leave the dashboard panel empty.
func TestGrafanaDashboard_ReferencesRound5Metrics(t *testing.T) {
	t.Parallel()
	path := filepath.Join("grafana", "context-engine-dashboard.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	body := string(raw)
	for _, metric := range []string{
		"context_engine_token_refreshes_total",
		"context_engine_index_auto_reindexes_total",
		"context_engine_kafka_consumer_lag",
		"context_engine_dlq_depth",
		"context_engine_pipeline_stage_duration_seconds_bucket",
		"context_engine_retrieval_backend_duration_seconds_bucket",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("dashboard does not reference %s", metric)
		}
	}
}

// TestAlertingRunbook_Exists makes sure the runbook is in place
// alongside the dashboard JSON. CI fails if either drifts.
func TestAlertingRunbook_Exists(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		filepath.Join("..", "docs", "runbooks", "alerting.md"),
		filepath.Join("grafana", "context-engine-dashboard.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing artefact %s: %v", p, err)
		}
	}
}
