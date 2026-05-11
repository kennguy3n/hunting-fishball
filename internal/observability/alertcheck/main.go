// Command alertcheck validates a Prometheus alerts YAML file.
//
// Round-4 Task 10: the manifest at deploy/alerts.yaml is checked
// in CI via `make alerts-check`. This binary parses the file and
// verifies:
//
//   - the document is valid YAML;
//   - the top-level kind is `PrometheusRule` (matching the
//     prometheus-operator CRD that the deploy targets);
//   - every alert rule names an `alert` and an `expr`;
//   - every alert carries a `severity` label and a `summary`
//     annotation. These are the two most operator-confusing
//     omissions when authoring rules by hand.
//
// We deliberately avoid validating the PromQL expressions — that
// requires a real Prometheus build context — but we catch the
// shape problems that broke previous deploys.
package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type rule struct {
	Alert string `yaml:"alert"`
	// Record is the Prometheus recording-rule name. Mutually
	// exclusive with Alert per the Prometheus spec; alertcheck
	// validates whichever is set.
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

type group struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval"`
	Rules    []rule `yaml:"rules"`
}

type manifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Spec       struct {
		Groups []group `yaml:"groups"`
	} `yaml:"spec"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: alertcheck <path> [<path> ...]")
		os.Exit(2)
	}
	var fail bool
	totalGroups, totalRules := 0, 0
	for _, path := range os.Args[1:] {
		g, r, ok := validateFile(path)
		totalGroups += g
		totalRules += r
		if !ok {
			fail = true
		}
	}
	if fail {
		os.Exit(1)
	}
	fmt.Printf("ok: %d groups, %d rules\n", totalGroups, totalRules)
}

// validateFile parses one PrometheusRule manifest and reports
// any shape problems. Returns (group count, rule count, ok).
func validateFile(path string) (int, int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: read: %v\n", path, err)
		return 0, 0, false
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		fmt.Fprintf(os.Stderr, "%s: yaml: %v\n", path, err)
		return 0, 0, false
	}
	if m.Kind != "PrometheusRule" {
		fmt.Fprintf(os.Stderr, "%s: kind=%q, want PrometheusRule\n", path, m.Kind)
		return 0, 0, false
	}
	if len(m.Spec.Groups) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no rule groups defined\n", path)
		return 0, 0, false
	}
	fail := false
	for gi, g := range m.Spec.Groups {
		if g.Name == "" {
			fmt.Fprintf(os.Stderr, "%s: group[%d]: missing name\n", path, gi)
			fail = true
		}
		for ri, r := range g.Rules {
			// Each rule must be EITHER an alert OR a recording rule.
			isAlert := r.Alert != ""
			isRecord := r.Record != ""
			if isAlert && isRecord {
				fmt.Fprintf(os.Stderr, "%s: group=%s rule[%d]: rule must be alert OR record, not both\n", path, g.Name, ri)
				fail = true
			}
			if !isAlert && !isRecord {
				fmt.Fprintf(os.Stderr, "%s: group=%s rule[%d]: missing alert or record\n", path, g.Name, ri)
				fail = true
				continue
			}
			name := r.Alert
			if isRecord {
				name = r.Record
			}
			if r.Expr == "" {
				fmt.Fprintf(os.Stderr, "%s: rule=%s: missing expr\n", path, name)
				fail = true
			}
			// Severity/summary annotations are mandatory only for
			// alerting rules — recording rules don't page anyone.
			if isAlert {
				if _, ok := r.Labels["severity"]; !ok {
					fmt.Fprintf(os.Stderr, "%s: alert=%s: missing labels.severity\n", path, name)
					fail = true
				}
				if _, ok := r.Annotations["summary"]; !ok {
					fmt.Fprintf(os.Stderr, "%s: alert=%s: missing annotations.summary\n", path, name)
					fail = true
				}
			}
		}
	}
	return len(m.Spec.Groups), countRules(m.Spec.Groups), !fail
}

func countRules(groups []group) int {
	n := 0
	for _, g := range groups {
		n += len(g.Rules)
	}
	return n
}
