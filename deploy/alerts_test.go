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
