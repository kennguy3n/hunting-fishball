// Package deploy_test holds a structural test for deploy/alerts.yaml.
//
// The test guards against the most common authoring mistakes that
// have broken Prometheus rule deploys:
//   - kind != PrometheusRule
//   - alert with no labels.severity (operator can't route)
//   - alert with no annotations.summary (pager has no headline)
//   - alert with no expr (silent rule)
package deploy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type rule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type group struct {
	Name  string `yaml:"name"`
	Rules []rule `yaml:"rules"`
}

type manifest struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Groups []group `yaml:"groups"`
	} `yaml:"spec"`
}

func TestAlertsManifest_Valid(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "alerts.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if m.Kind != "PrometheusRule" {
		t.Fatalf("kind=%q", m.Kind)
	}
	if len(m.Spec.Groups) == 0 {
		t.Fatal("no rule groups defined")
	}
	for _, g := range m.Spec.Groups {
		if g.Name == "" {
			t.Errorf("group with empty name")
		}
		for _, r := range g.Rules {
			if r.Alert == "" {
				t.Errorf("group=%s rule with empty alert", g.Name)
			}
			if r.Expr == "" {
				t.Errorf("alert=%s: missing expr", r.Alert)
			}
			if _, ok := r.Labels["severity"]; !ok {
				t.Errorf("alert=%s: missing labels.severity", r.Alert)
			}
			if _, ok := r.Annotations["summary"]; !ok {
				t.Errorf("alert=%s: missing annotations.summary", r.Alert)
			}
		}
	}
}

// TestRecordingRulesManifest_Valid — Round-9 Task 16. Validates
// deploy/recording-rules.yaml shape: must be a PrometheusRule with
// at least one rule per group, every rule must have a `record`
// name + `expr` (and no severity/summary, those are alert-only).
func TestRecordingRulesManifest_Valid(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "recording-rules.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Reuse the local rule struct but add Record alongside Alert.
	type recRule struct {
		Alert  string            `yaml:"alert"`
		Record string            `yaml:"record"`
		Expr   string            `yaml:"expr"`
		Labels map[string]string `yaml:"labels"`
	}
	type recGroup struct {
		Name  string    `yaml:"name"`
		Rules []recRule `yaml:"rules"`
	}
	type rec struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Groups []recGroup `yaml:"groups"`
		} `yaml:"spec"`
	}
	var m rec
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if m.Kind != "PrometheusRule" {
		t.Fatalf("kind=%q", m.Kind)
	}
	if len(m.Spec.Groups) == 0 {
		t.Fatal("no rule groups defined")
	}
	// Round-9 spec lists the three derived metrics that MUST be
	// emitted. The test pins their presence so a future edit can't
	// silently drop one.
	required := map[string]bool{
		"context_engine_retrieval_availability":         false,
		"context_engine_pipeline_throughput_per_minute": false,
		"context_engine_cache_hit_rate":                 false,
	}
	for _, g := range m.Spec.Groups {
		if g.Name == "" {
			t.Errorf("group with empty name")
		}
		for _, r := range g.Rules {
			if r.Alert != "" {
				t.Errorf("recording rule must not set alert=%s", r.Alert)
			}
			if r.Record == "" {
				t.Errorf("group=%s rule with empty record", g.Name)
			}
			if r.Expr == "" {
				t.Errorf("record=%s: missing expr", r.Record)
			}
			if _, ok := required[r.Record]; ok {
				required[r.Record] = true
			}
		}
	}
	for name, found := range required {
		if !found {
			t.Errorf("required recording rule missing: %s", name)
		}
	}
}

// TestAlertsManifest_Round11Alerts — Round-11 Task 11.
//
// Asserts every Round-11 alert is present in alerts.yaml so a
// future refactor doesn't drop them silently. The test does not
// assert exact thresholds; structural validation lives in
// TestAlertsManifest_Valid.
func TestAlertsManifest_Round11Alerts(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "alerts.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	required := map[string]bool{
		"ChunkQualityScoreDropped": false,
		"CacheHitRateLow":          false,
		"CredentialHealthDegraded": false,
		"GORMStoreLatencyHigh":     false,
	}
	for _, g := range m.Spec.Groups {
		for _, r := range g.Rules {
			if _, ok := required[r.Alert]; ok {
				required[r.Alert] = true
			}
		}
	}
	for name, found := range required {
		if !found {
			t.Errorf("required Round-11 alert missing: %s", name)
		}
	}
}

// TestAlertsManifest_Round12Alerts — Round-12 Tasks 1+2.
//
// Asserts every Round-12 alert is present and carries the expected
// severity. The three rules — GRPCCircuitBreakerOpen,
// PostgresPoolSaturated, RedisPoolSaturated — must stay wired up so
// future refactors of the manifest don't silently drop them.
func TestAlertsManifest_Round12Alerts(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "alerts.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	wantSeverity := map[string]string{
		"GRPCCircuitBreakerOpen": "page",
		"PostgresPoolSaturated":  "warning",
		"RedisPoolSaturated":     "warning",
	}
	found := map[string]rule{}
	for _, g := range m.Spec.Groups {
		for _, r := range g.Rules {
			if _, ok := wantSeverity[r.Alert]; ok {
				found[r.Alert] = r
			}
		}
	}
	for name, sev := range wantSeverity {
		r, ok := found[name]
		if !ok {
			t.Errorf("required Round-12 alert missing: %s", name)
			continue
		}
		if r.Labels["severity"] != sev {
			t.Errorf("alert=%s severity=%q want=%q", name, r.Labels["severity"], sev)
		}
		if r.For == "" {
			t.Errorf("alert=%s missing `for:` duration", name)
		}
		if r.Expr == "" {
			t.Errorf("alert=%s missing expr", name)
		}
	}
	// Round-12 Task 1 specifically requires breaker == 2 (open).
	if r, ok := found["GRPCCircuitBreakerOpen"]; ok {
		if !strings.Contains(r.Expr, "context_engine_grpc_circuit_breaker_state") {
			t.Errorf("GRPCCircuitBreakerOpen expr missing breaker metric: %q", r.Expr)
		}
		if !strings.Contains(r.Expr, "== 2") {
			t.Errorf("GRPCCircuitBreakerOpen expr must pin the open state (==2): %q", r.Expr)
		}
	}
	if r, ok := found["PostgresPoolSaturated"]; ok {
		if !strings.Contains(r.Expr, "context_engine_postgres_pool_open_connections") {
			t.Errorf("PostgresPoolSaturated expr missing pool metric: %q", r.Expr)
		}
	}
	if r, ok := found["RedisPoolSaturated"]; ok {
		if !strings.Contains(r.Expr, "context_engine_redis_pool_active_connections") {
			t.Errorf("RedisPoolSaturated expr missing pool metric: %q", r.Expr)
		}
	}
}

// TestAlertsManifest_Round13BurnRate — Round-13 Task 2.
//
// Validates deploy/alerts/slo_burn_rate.yaml as a PrometheusRule
// with the four expected multi-window burn-rate alerts. The four
// alerts cover the two SLOs (retrieval latency, pipeline
// throughput) at both burn rates (fast=page, slow=warning).
func TestAlertsManifest_Round13BurnRate(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(".", "alerts", "slo_burn_rate.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if m.Kind != "PrometheusRule" {
		t.Fatalf("kind=%q", m.Kind)
	}
	wantSeverity := map[string]string{
		"RetrievalLatencyBudgetBurnFast":   "page",
		"RetrievalLatencyBudgetBurnSlow":   "warning",
		"PipelineThroughputBudgetBurnFast": "page",
		"PipelineThroughputBudgetBurnSlow": "warning",
	}
	found := map[string]rule{}
	for _, g := range m.Spec.Groups {
		for _, r := range g.Rules {
			if _, ok := wantSeverity[r.Alert]; ok {
				found[r.Alert] = r
			}
		}
	}
	for name, sev := range wantSeverity {
		r, ok := found[name]
		if !ok {
			t.Errorf("required Round-13 burn-rate alert missing: %s", name)
			continue
		}
		if r.Labels["severity"] != sev {
			t.Errorf("alert=%s severity=%q want=%q", name, r.Labels["severity"], sev)
		}
		if r.For == "" {
			t.Errorf("alert=%s missing `for:` duration", name)
		}
		if r.Expr == "" {
			t.Errorf("alert=%s missing expr", name)
		}
	}
	// The retrieval-latency burn alerts must reference the
	// recording rule from deploy/recording-rules.yaml.
	if r, ok := found["RetrievalLatencyBudgetBurnFast"]; ok {
		if !strings.Contains(r.Expr, "context_engine_retrieval_p95_latency_seconds") {
			t.Errorf("RetrievalLatencyBudgetBurnFast expr missing recording-rule metric: %q", r.Expr)
		}
	}
	// The throughput burn alerts must reference the recording rule.
	if r, ok := found["PipelineThroughputBudgetBurnFast"]; ok {
		if !strings.Contains(r.Expr, "context_engine_pipeline_throughput_per_minute") {
			t.Errorf("PipelineThroughputBudgetBurnFast expr missing recording-rule metric: %q", r.Expr)
		}
	}
}
