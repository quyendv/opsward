package app

import (
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/policy"
	"github.com/quyendv/opsward/internal/provider/kubernetes"
)

// grantees returns, per cluster and profile, the principals a call through the profile
// can act for (docs/design/0014). Besides what policy grants through a profile: the
// reader looks up labels for every call when policy reads them, and the delivery profile
// reads Applications for every caller of a delivery tool.
func grantees(cfg *config.Config, pol *policy.Policy, cat policy.Catalog) map[string]map[string]policy.Grantees {
	out := map[string]map[string]policy.Grantees{}
	merge := func(cluster, profile string, g policy.Grantees) {
		if out[cluster] == nil {
			out[cluster] = map[string]policy.Grantees{}
		}
		out[cluster][profile] = out[cluster][profile].Merge(g)
	}
	var everyone policy.Grantees
	for env, profiles := range pol.Grantees(cat) {
		cluster := ""
		for _, e := range cfg.Environments {
			if e.Name == env {
				cluster = e.Cluster
			}
		}
		for profile, g := range profiles {
			merge(cluster, profile, g)
			everyone = everyone.Merge(g)
			if pol.NeedsLabels() {
				merge(cluster, core.ProfileReader, g)
			}
		}
	}
	if d := cfg.Providers.Delivery; d != nil {
		merge(d.Cluster, d.Profile, everyone)
	}
	return out
}

// impersonation is what a profile's client needs to act as its callers; nil for a
// profile that acts as itself.
func impersonation(i *config.Impersonate, g policy.Grantees) *kubernetes.Impersonation {
	if i == nil {
		return nil
	}
	return &kubernetes.Impersonation{UserPrefix: i.UserPrefix(), GroupPrefix: i.GroupPrefix(), Groups: g.Groups}
}
