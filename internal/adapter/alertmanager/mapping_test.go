package alertmanager

import (
	"strings"
	"testing"
)

var envs = []Environment{
	{Name: "staging", ClusterLabelValue: "cluster-a", Namespaces: []string{"team-a-*"}},
	{Name: "staging-shared", ClusterLabelValue: "cluster-a", Namespaces: []string{"shared"}},
	{Name: "production", ClusterLabelValue: "cluster-b", Namespaces: []string{"*"}},
}

var rules = Mapping{
	EnvironmentLabel: "cluster", NamespaceLabel: "namespace",
	WorkloadLabels: []WorkloadLabel{{Label: "deployment", Kind: "Deployment"}, {Label: "statefulset", Kind: "StatefulSet"}},
}

func TestMapAlertToWorkload(t *testing.T) {
	tests := []struct {
		labels map[string]string
		want   Target
		err    string
	}{
		{map[string]string{"cluster": "cluster-a", "namespace": "team-a-api", "deployment": "api"}, Target{"staging", "team-a-api", "Deployment", "api"}, ""},
		{map[string]string{"cluster": "cluster-a", "namespace": "shared", "statefulset": "db"}, Target{"staging-shared", "shared", "StatefulSet", "db"}, ""},
		{map[string]string{"cluster": "cluster-b", "namespace": "payments", "deployment": "api", "statefulset": "x"}, Target{"production", "payments", "Deployment", "api"}, ""},
		{map[string]string{"cluster": "cluster-a", "namespace": "kube-system", "deployment": "coredns"}, Target{}, "no environment"},
		{map[string]string{"cluster": "cluster-z", "namespace": "team-a-api", "deployment": "api"}, Target{}, "no environment"},
		{map[string]string{"cluster": "cluster-a", "namespace": "team-a-api", "pod": "api-1"}, Target{}, "no workload label"},
		{map[string]string{"cluster": "cluster-a", "deployment": "api"}, Target{}, "no namespace label"},
	}
	for _, tt := range tests {
		got, err := rules.Map(tt.labels, envs)
		if tt.err == "" && (err != nil || got != tt.want) {
			t.Errorf("Map(%v) = %+v, %v; want %+v", tt.labels, got, err, tt.want)
		}
		if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("Map(%v) err = %v; want %q", tt.labels, err, tt.err)
		}
	}
}

// Without an environment label, the namespace decides, but only when exactly one
// environment covers it.
func TestMapByNamespaceAlone(t *testing.T) {
	m := rules
	m.EnvironmentLabel = ""
	if got, err := m.Map(map[string]string{"namespace": "shared", "deployment": "x"}, envs[:2]); err != nil || got.Environment != "staging-shared" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := m.Map(map[string]string{"namespace": "shared", "deployment": "x"}, envs); err == nil || !strings.Contains(err.Error(), "several environments") {
		t.Fatalf("ambiguous: err = %v", err)
	}
}
