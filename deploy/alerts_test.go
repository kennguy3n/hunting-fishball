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
