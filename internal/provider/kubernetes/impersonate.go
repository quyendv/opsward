package kubernetes

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/version"
)

// Impersonation makes a profile act as the caller of each request (docs/design/0014).
type Impersonation struct {
	UserPrefix  string
	GroupPrefix string
	// Groups are the principal groups that may be sent, unprefixed: those policy names for
	// the profile. The credential may impersonate only listed groups, and one it may not
	// fails the whole request.
	Groups []string
}

func (i *Impersonation) equal(o *Impersonation) bool {
	if i == nil || o == nil {
		return i == o
	}
	return i.UserPrefix == o.UserPrefix && i.GroupPrefix == o.GroupPrefix && slices.Equal(i.Groups, o.Groups)
}

func (p Profile) equal(o Profile) bool {
	return p.Kubeconfig == o.Kubeconfig && p.Context == o.Context && p.Impersonate.equal(o.Impersonate)
}

// restConfig reads a profile's kubeconfig. It never falls back to in-cluster
// credentials, which client-go's deferred loader does for an empty file: a profile must
// not silently borrow the pod's own ServiceAccount.
func restConfig(p Profile) (*rest.Config, error) {
	raw, err := clientcmd.LoadFromFile(p.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if len(raw.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig %s: no contexts", p.Kubeconfig)
	}
	if p.Context == "" && raw.CurrentContext == "" {
		return nil, fmt.Errorf("kubeconfig %s: no current-context; set the profile's context field", p.Kubeconfig)
	}
	cfg, err := clientcmd.NewNonInteractiveClientConfig(*raw, p.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", p.Kubeconfig, err)
	}
	cfg.UserAgent = "opsward/" + version.Version
	if p.Impersonate != nil {
		imp := cfg.Impersonate
		if imp.UserName != "" || imp.UID != "" || len(imp.Groups) > 0 || len(imp.Extra) > 0 {
			return nil, fmt.Errorf("kubeconfig %s: it sets impersonation itself, and the profile impersonates the caller", p.Kubeconfig)
		}
		i := *p.Impersonate
		cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &asCaller{rt: rt, imp: i} })
	}
	return cfg, nil
}

var errNoCaller = errors.New("kubernetes: this profile impersonates the caller, and the request has no caller")

// asCaller sets the impersonation headers from the principal of each request's context.
type asCaller struct {
	rt  http.RoundTripper
	imp Impersonation
}

func (a *asCaller) RoundTrip(req *http.Request) (*http.Response, error) {
	user, groups, err := a.names(req)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	req = utilnet.CloneRequest(req)
	req.Header.Set(transport.ImpersonateUserHeader, user)
	for _, g := range groups {
		req.Header.Add(transport.ImpersonateGroupHeader, g)
	}
	return a.rt.RoundTrip(req)
}

func (a *asCaller) names(req *http.Request) (string, []string, error) {
	p, ok := core.PrincipalFrom(req.Context())
	if !ok || p.Subject == "" {
		return "", nil, errNoCaller
	}
	user := a.imp.UserPrefix + core.NormalizeSubject(p.Subject)
	if strings.HasPrefix(user, "system:") {
		return "", nil, fmt.Errorf("kubernetes: refusing to impersonate %q: system: names are the cluster's own", user)
	}
	var groups []string
	for _, g := range p.Groups {
		if !slices.Contains(a.imp.Groups, g) {
			continue
		}
		name := a.imp.GroupPrefix + g
		if strings.HasPrefix(name, "system:") {
			return "", nil, fmt.Errorf("kubernetes: refusing to impersonate group %q: system: names are the cluster's own", name)
		}
		if !slices.Contains(groups, name) {
			groups = append(groups, name)
		}
	}
	slices.Sort(groups)
	return user, groups, nil
}

func (a *asCaller) WrappedRoundTripper() http.RoundTripper { return a.rt }
