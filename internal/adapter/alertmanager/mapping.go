// Package alertmanager receives Alertmanager's webhook and investigates each firing alert
// with diagnose_workload through the engine, then notifies (docs/design/0012).
package alertmanager

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Environment is what mapping needs of a configured environment.
type Environment struct {
	Name              string
	ClusterLabelValue string
	Namespaces        []string // patterns
}

// WorkloadLabel names the alert label that carries a workload of Kind.
type WorkloadLabel struct {
	Label, Kind string
}

// Mapping finds an alert's workload from its labels.
type Mapping struct {
	// EnvironmentLabel's value is matched against environments' cluster label values;
	// empty, the namespace alone decides.
	EnvironmentLabel string
	NamespaceLabel   string
	WorkloadLabels   []WorkloadLabel // tried in order
}

// Target is the workload an alert is about.
type Target struct {
	Environment, Namespace, Kind, Name string
}

// ErrUnmapped is wrapped by Map when an alert names no single workload.
var ErrUnmapped = errors.New("alert not mapped to a workload")

// Map returns the workload the labels name.
func (m Mapping) Map(labels map[string]string, envs []Environment) (Target, error) {
	ns := labels[m.NamespaceLabel]
	if ns == "" {
		return Target{}, fmt.Errorf("%w: no namespace label %q", ErrUnmapped, m.NamespaceLabel)
	}
	t := Target{Namespace: ns}
	for _, wl := range m.WorkloadLabels {
		if v := labels[wl.Label]; v != "" {
			t.Kind, t.Name = wl.Kind, v
			break
		}
	}
	if t.Name == "" {
		names := make([]string, len(m.WorkloadLabels))
		for i, wl := range m.WorkloadLabels {
			names[i] = wl.Label
		}
		return Target{}, fmt.Errorf("%w: no workload label (%s)", ErrUnmapped, strings.Join(names, ", "))
	}
	// Names reach the engine's arguments and the audit record; Kubernetes names are at
	// most 253 bytes.
	for _, v := range []string{t.Namespace, t.Name} {
		if len(v) > 253 {
			return Target{}, fmt.Errorf("%w: a label value is longer than 253 bytes", ErrUnmapped)
		}
	}
	var matched []string
	for _, e := range envs {
		if m.EnvironmentLabel != "" && labels[m.EnvironmentLabel] != e.ClusterLabelValue {
			continue
		}
		if covers(e.Namespaces, ns) {
			matched = append(matched, e.Name)
		}
	}
	switch len(matched) {
	case 0:
		return Target{}, fmt.Errorf("%w: no environment covers namespace %s%s", ErrUnmapped, ns, m.clusterHint(labels))
	case 1:
		t.Environment = matched[0]
		return t, nil
	}
	return Target{}, fmt.Errorf("%w: several environments cover namespace %s%s: %s", ErrUnmapped, ns, m.clusterHint(labels), strings.Join(matched, ", "))
}

func (m Mapping) clusterHint(labels map[string]string) string {
	if m.EnvironmentLabel == "" {
		return ""
	}
	return fmt.Sprintf(" with %s=%q", m.EnvironmentLabel, labels[m.EnvironmentLabel])
}

func covers(patterns []string, ns string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, ns); ok {
			return true
		}
	}
	return false
}
