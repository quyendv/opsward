package app

import (
	"net/http"

	"github.com/quyendv/opsward/internal/adapter/console"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/policy"
)

// Console returns the web console's handler, to serve at /console/, or nil when the
// configuration has no console section (docs/design/0015).
func (a *App) Console() (http.Handler, error) {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	k := cfg.Console
	if k == nil {
		return nil, nil
	}
	opt := console.Options{
		Engine: a.Engine, Identity: a.Identity, Sessions: a.Store, Naming: a.naming,
		APITokens: k.APITokens, SessionTTL: k.SessionTTL.Std(), Logger: a.log,
	}
	if o := k.OIDC; o != nil {
		var secret string
		if o.ClientSecret != nil {
			var err error
			if secret, err = o.ClientSecret.Resolve("console.oidc.clientSecret"); err != nil {
				return nil, err
			}
		}
		opt.OIDC = &console.OIDC{ClientID: o.ClientID, ClientSecret: secret, Scopes: o.Scopes, RedirectURL: k.RedirectURL(cfg.Server.HTTP.PublicURL)}
	}
	return console.New(opt)
}

// naming answers from the policy in force.
func (a *App) naming(p core.Principal) (grants, approves []policy.BindingView) {
	return a.policy.Load().Naming(p)
}
