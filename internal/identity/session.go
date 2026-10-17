package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/quyendv/opsward/internal/core"
)

// Identity is who a credential names, without internal groups: what a console session
// keeps (docs/design/0015).
type Identity struct {
	Subject    string
	Groups     []string // the issuer's, prefixed; empty for an API token, whose groups are read again
	Credential string   // as core.Principal.Credential
	Expires    time.Time
}

// Principal returns the principal of an identity a session kept, with the current
// internal groups. An API token is read again, and must still be valid and name the
// same subject.
func (a *Authenticator) Principal(ctx context.Context, id Identity) (core.Principal, error) {
	subject := core.NormalizeSubject(id.Subject)
	groups := id.Groups
	if tid, ok := strings.CutPrefix(id.Credential, "token:"); ok {
		t, found, err := a.store.APIToken(ctx, tid)
		if err != nil {
			return core.Principal{}, err
		}
		switch {
		case !found:
			return core.Principal{}, unauthenticated("unknown token " + tid)
		case !t.RevokedAt.IsZero():
			return core.Principal{}, unauthenticated("token " + tid + " is revoked")
		case !a.now().Before(t.ExpiresAt):
			return core.Principal{}, unauthenticated("token " + tid + " expired")
		case core.NormalizeSubject(t.Subject) != subject:
			return core.Principal{}, unauthenticated("token " + tid + " names another subject")
		}
		if now := a.now(); now.Sub(t.LastUsedAt) >= touchInterval {
			if err := a.store.TouchAPIToken(ctx, tid, now); err != nil {
				a.log.Warn("recording API token use failed", "token", tid, "error", err)
			}
		}
		groups = t.Groups
	}
	return core.Principal{Subject: subject, Groups: a.groups(subject, groups), Credential: id.Credential}, nil
}

// Endpoints returns the issuer's authorization and token endpoints, once discovered.
func (a *Authenticator) Endpoints() (authURL, tokenURL string, ok bool) {
	if a.oidc == nil {
		return "", "", false
	}
	is := a.oidc.issued.Load()
	if is == nil {
		return "", "", false
	}
	return is.authURL, is.tokenURL, true
}

// VerifyIDToken checks an ID token the issuer gave clientID for a sign-in that sent
// nonce, and maps its claims as an access token's are.
func (a *Authenticator) VerifyIDToken(ctx context.Context, raw, clientID, nonce string) (Identity, error) {
	if a.oidc == nil {
		return Identity{}, errors.New("OIDC is not configured")
	}
	is := a.oidc.issued.Load()
	if is == nil {
		return Identity{}, errors.New("OIDC issuer not discovered yet")
	}
	v := oidc.NewVerifier(a.oidc.opt.Issuer, is.keys, &oidc.Config{ClientID: clientID, Now: a.now, SupportedSigningAlgs: is.algs})
	var keys keyStatus
	tok, err := v.Verify(context.WithValue(ctx, keyStatusKey{}, &keys), raw)
	if err != nil && keys.unavailable != nil {
		return Identity{}, fmt.Errorf("ID token cannot be checked: %w", keys.unavailable)
	}
	if err != nil {
		return Identity{}, unauthenticated("ID token: " + err.Error())
	}
	if nonce == "" || tok.Nonce != nonce {
		return Identity{}, unauthenticated("ID token nonce does not match the sign-in")
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, unauthenticated("ID token claims: " + err.Error())
	}
	subject, groups, err := a.oidc.names(claims)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Subject: subject, Groups: groups, Credential: "oidc", Expires: tok.Expiry}, nil
}
