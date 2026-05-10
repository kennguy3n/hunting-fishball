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
	Alert       string            `yaml:"alert"`
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
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: alertcheck <path>")
		os.Exit(2)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	var m manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		fmt.Fprintf(os.Stderr, "yaml: %v\n", err)
		os.Exit(1)
	}
	if m.Kind != "PrometheusRule" {
		fmt.Fprintf(os.Stderr, "kind=%q, want PrometheusRule\n", m.Kind)
		os.Exit(1)
	}
	if len(m.Spec.Groups) == 0 {
		fmt.Fprintln(os.Stderr, "no rule groups defined")
		os.Exit(1)
	}
	var fail bool
	for gi, g := range m.Spec.Groups {
		if g.Name == "" {
			fmt.Fprintf(os.Stderr, "group[%d]: missing name\n", gi)
			fail = true
		}
		for ri, r := range g.Rules {
			if r.Alert == "" {
				fmt.Fprintf(os.Stderr, "group=%s rule[%d]: missing alert\n", g.Name, ri)
				fail = true
			}
			if r.Expr == "" {
				fmt.Fprintf(os.Stderr, "alert=%s: missing expr\n", r.Alert)
				fail = true
			}
			if _, ok := r.Labels["severity"]; !ok {
				fmt.Fprintf(os.Stderr, "alert=%s: missing labels.severity\n", r.Alert)
				fail = true
			}
			if _, ok := r.Annotations["summary"]; !ok {
				fmt.Fprintf(os.Stderr, "alert=%s: missing annotations.summary\n", r.Alert)
				fail = true
			}
		}
	}
	if fail {
		os.Exit(1)
	}
	fmt.Printf("ok: %d groups, %d rules\n", len(m.Spec.Groups), countRules(m.Spec.Groups))
}

func countRules(groups []group) int {
	n := 0
	for _, g := range groups {
		n += len(g.Rules)
	}
	return n
}
