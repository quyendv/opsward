package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"

	"github.com/quyendv/opsward/internal/core"
)

// OIDCOptions configures access tokens from an OpenID Connect issuer. docs/design/0005.
type OIDCOptions struct {
	Issuer       string
	Audience     string
	SubjectClaim string
	GroupsClaim  string
	GroupsPrefix string
	// RetryInterval is the first wait after a failed discovery; it doubles up to a
	// minute. Default one second.
	RetryInterval time.Duration
	// KeyRefreshInterval is the shortest time between two fetches of the issuer's keys
	// for a token naming a key ID not seen yet. Default 30 seconds.
	KeyRefreshInterval time.Duration
}

const (
	maxRetryInterval = time.Minute
	issuerTimeout    = 10 * time.Second
)

type oidcState struct {
	opt      OIDCOptions
	verifier atomic.Pointer[oidc.IDTokenVerifier]
	// issued are what verifying an ID token for another client needs, and where the
	// console signs people in (docs/design/0015). Set with verifier.
	issued atomic.Pointer[issued]
}

type issued struct {
	keys     *keySet
	algs     []string
	authURL  string
	tokenURL string
}

func (a *Authenticator) oidcReady() bool { return a.oidc != nil && a.oidc.verifier.Load() != nil }

// Discover reads the issuer's discovery document, retrying until it succeeds or ctx is
// done. Until then OIDC tokens cannot be checked and API tokens work.
func (a *Authenticator) Discover(ctx context.Context) {
	if a.oidc == nil {
		return
	}
	o := a.oidc
	client := &http.Client{Timeout: issuerTimeout}
	ctx = oidc.ClientContext(ctx, client)
	wait := o.opt.RetryInterval
	if wait <= 0 {
		wait = time.Second
	}
	for {
		p, err := oidc.NewProvider(ctx, o.opt.Issuer)
		if err == nil {
			var meta struct {
				JWKSURI string   `json:"jwks_uri"`
				Algs    []string `json:"id_token_signing_alg_values_supported"`
			}
			if err = p.Claims(&meta); err == nil && meta.JWKSURI == "" {
				err = errors.New("discovery document has no jwks_uri")
			}
			if err == nil {
				v, is := o.newVerifier(client, meta.JWKSURI, meta.Algs, a.now)
				ep := p.Endpoint()
				is.authURL, is.tokenURL = ep.AuthURL, ep.TokenURL
				o.issued.Store(is)
				o.verifier.Store(v)
				a.log.Info("OIDC issuer discovered", "issuer", o.opt.Issuer)
				return
			}
		}
		var mismatch *oidc.IssuerMismatchError
		if errors.As(err, &mismatch) {
			// Retrying will not fix a configuration that names the issuer differently.
			a.log.Error("the OIDC issuer names itself differently from identity.oidc.issuer; set it exactly to the advertised value",
				"configured", mismatch.Provided, "advertised", mismatch.Discovered, "retry", wait)
		} else {
			a.log.Warn("OIDC issuer discovery failed; OIDC tokens cannot be checked until it succeeds", "issuer", o.opt.Issuer, "error", err, "retry", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, maxRetryInterval)
	}
}

func (o *oidcState) newVerifier(client *http.Client, jwksURI string, algNames []string, now func() time.Time) (*oidc.IDTokenVerifier, *issued) {
	if len(algNames) == 0 {
		algNames = []string{oidc.RS256}
	}
	algs := make([]jose.SignatureAlgorithm, 0, len(algNames))
	for _, a := range algNames {
		if a != "none" && !strings.HasPrefix(a, "HS") { // never symmetric or unsigned
			algs = append(algs, jose.SignatureAlgorithm(a))
		}
	}
	refresh := o.opt.KeyRefreshInterval
	if refresh <= 0 {
		refresh = defaultKeyRefreshInterval
	}
	ks := &keySet{url: jwksURI, client: client, algs: algs, minRefresh: refresh, now: now}
	names := make([]string, len(algs))
	for i, a := range algs {
		names[i] = string(a)
	}
	return oidc.NewVerifier(o.opt.Issuer, ks, &oidc.Config{ClientID: o.opt.Audience, Now: now, SupportedSigningAlgs: names}),
		&issued{keys: ks, algs: names}
}

func (a *Authenticator) authenticateOIDC(ctx context.Context, raw string) (core.Principal, time.Time, error) {
	v := a.oidc.verifier.Load()
	if v == nil {
		// Not a rejection: the token may be valid, and a 401 would send the client back
		// through sign-in.
		return core.Principal{}, time.Time{}, errors.New("OIDC issuer not discovered yet")
	}
	if strings.Count(raw, ".") != 2 {
		return core.Principal{}, time.Time{}, unauthenticated("malformed token: neither an API token nor a JWT")
	}
	if typ := headerType(raw); !accessTokenType(typ) {
		return core.Principal{}, time.Time{}, unauthenticated(fmt.Sprintf("OIDC token type %q is not an access token", typ))
	}
	var keys keyStatus
	tok, err := v.Verify(context.WithValue(ctx, keyStatusKey{}, &keys), raw)
	if err != nil && keys.unavailable != nil {
		return core.Principal{}, time.Time{}, fmt.Errorf("OIDC token cannot be checked: %w", keys.unavailable)
	}
	if err != nil {
		var expired *oidc.TokenExpiredError
		if errors.As(err, &expired) {
			return core.Principal{}, time.Time{}, unauthenticated("OIDC token expired")
		}
		return core.Principal{}, time.Time{}, unauthenticated("OIDC token: " + err.Error())
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return core.Principal{}, time.Time{}, unauthenticated("OIDC token claims: " + err.Error())
	}
	if typ, ok := claims["typ"].(string); ok && !strings.EqualFold(typ, "Bearer") && !accessTokenType(typ) {
		return core.Principal{}, time.Time{}, unauthenticated(fmt.Sprintf("OIDC token typ claim %q is not an access token", typ))
	}
	subject, groups, err := a.oidc.names(claims)
	if err != nil {
		return core.Principal{}, time.Time{}, err
	}
	return core.Principal{Subject: subject, Groups: a.groups(subject, groups), Credential: "oidc"}, tok.Expiry, nil
}

// names maps a token's claims to a subject and the issuer's groups, prefixed.
func (o *oidcState) names(claims map[string]any) (string, []string, error) {
	subject, _ := claims[o.opt.SubjectClaim].(string)
	subject = core.NormalizeSubject(subject)
	if subject == "" {
		return "", nil, unauthenticated(fmt.Sprintf("OIDC token has no %q claim", o.opt.SubjectClaim))
	}
	if o.opt.SubjectClaim == "email" && !verified(claims["email_verified"]) {
		return "", nil, unauthenticated("OIDC token email_verified is not true")
	}
	groups := claimStrings(claims[o.opt.GroupsClaim])
	for i, g := range groups {
		groups[i] = o.opt.GroupsPrefix + g
	}
	return subject, groups, nil
}

// headerType returns the typ of a JWT's header, or "" when it has none or cannot be read.
func headerType(raw string) string {
	b, err := base64.RawURLEncoding.DecodeString(raw[:strings.IndexByte(raw, '.')])
	if err != nil {
		return ""
	}
	var h struct {
		Typ string `json:"typ"`
	}
	_ = json.Unmarshal(b, &h)
	return h.Typ
}

// accessTokenType accepts the JWT types an access token can carry (RFC 9068), and none.
func accessTokenType(typ string) bool {
	switch strings.ToLower(typ) {
	case "", "jwt", "at+jwt", "application/at+jwt":
		return true
	}
	return false
}

// verified accepts true, and the string "true" that some issuers send.
func verified(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// claimStrings reads a claim that is a string or a list; other values are ignored.
func claimStrings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
