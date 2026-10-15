package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/identity/oidctest"
)

const audience = "https://opsward.example.com/mcp"

func claims(is *oidctest.Issuer, over map[string]any) map[string]any {
	return is.Claims(audience, over)
}

func oidcAuth(t *testing.T, is *oidctest.Issuer, opt OIDCOptions) *Authenticator {
	t.Helper()
	opt.Issuer, opt.Audience = is.URL, audience
	if opt.SubjectClaim == "" {
		opt.SubjectClaim = "email"
	}
	if opt.GroupsClaim == "" {
		opt.GroupsClaim = "groups"
	}
	opt.RetryInterval = 10 * time.Millisecond
	a := New(Options{Store: openStore(t), OIDC: &opt, Groups: map[string][]string{"platform": {"alice@example.com"}}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Discover(ctx)
	return a
}

// ready waits for discovery.
func ready(t *testing.T, a *Authenticator) {
	t.Helper()
	for range 200 {
		if a.oidcReady() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("discovery did not complete")
}

func TestOIDCAuthenticates(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{GroupsPrefix: "oidc:"})
	ready(t, a)
	p, exp, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil)))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != "alice@example.com" || p.Credential != "oidc" {
		t.Fatalf("principal = %+v", p)
	}
	if !slices.Equal(p.Groups, []string{"oidc:admins", "oidc:team-a", "platform"}) {
		t.Fatalf("groups = %v, want prefixed claim groups and internal groups", p.Groups)
	}
	if time.Until(exp) < 59*time.Minute {
		t.Fatalf("expiry = %v", exp)
	}
}

func TestOIDCRejects(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{})
	ready(t, a)
	other := oidctest.Key(t)
	for name, tt := range map[string]struct {
		token  string
		reason string
	}{
		"wrong audience":     {is.Sign(claims(is, map[string]any{"aud": "some-client"})), "audience"},
		"expired":            {is.Sign(claims(is, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})), "expired"},
		"wrong issuer":       {is.Sign(claims(is, map[string]any{"iss": "https://evil.example.com"})), "different provider"},
		"forged signature":   {oidctest.SignWith(t, other, "k1", claims(is, nil)), "signature"},
		"unverified email":   {is.Sign(claims(is, map[string]any{"email_verified": false})), "email_verified"},
		"no email_verified":  {is.Sign(claims(is, map[string]any{"email_verified": nil})), "email_verified"},
		"no subject claim":   {is.Sign(claims(is, map[string]any{"email": nil})), `no "email" claim`},
		"non-string subject": {is.Sign(claims(is, map[string]any{"email": 42})), `no "email" claim`},
		"not a jwt":          {"garbage", "malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := a.Authenticate(context.Background(), tt.token)
			if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("err = %v, want ErrUnauthenticated mentioning %q", err, tt.reason)
			}
		})
	}
}

func TestOIDCClaimShapes(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{SubjectClaim: "sub", GroupsClaim: "roles"})
	ready(t, a)
	for name, tt := range map[string]struct {
		over map[string]any
		want []string
	}{
		"single string": {map[string]any{"roles": "ops"}, []string{"ops"}},
		"list":          {map[string]any{"roles": []string{"b", "a"}}, []string{"a", "b"}},
		"absent":        {nil, nil},
		"non-strings":   {map[string]any{"roles": []any{"a", 7}}, []string{"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			// With sub as the subject, email_verified is not required.
			p, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, merge(tt.over, map[string]any{"email_verified": nil}))))
			if err != nil {
				t.Fatal(err)
			}
			if p.Subject != "u-1" || !slices.Equal(p.Groups, tt.want) {
				t.Fatalf("principal = %+v, want groups %v", p, tt.want)
			}
		})
	}
}

func merge(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// An issuer that is down at start-up does not stop API tokens, and OIDC works once it
// can be reached.
func TestOIDCDiscoveryRetries(t *testing.T) {
	is := oidctest.New(t)
	is.Down.Store(true)
	a := oidcAuth(t, is, OIDCOptions{})
	tok := is.Sign(claims(is, nil))
	// Undecidable, not rejected: a 401 would send the client back through sign-in.
	if _, _, err := a.Authenticate(context.Background(), tok); err == nil || errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "not discovered") {
		t.Fatalf("before discovery: %v", err)
	}
	apiTok, _ := create(t, a, "ci@example.com", nil)
	if _, _, err := a.Authenticate(context.Background(), apiTok); err != nil {
		t.Fatalf("API token while the issuer is down: %v", err)
	}
	is.Down.Store(false)
	ready(t, a)
	if _, _, err := a.Authenticate(context.Background(), tok); err != nil {
		t.Fatalf("after discovery: %v", err)
	}
}

// A rotated signing key is fetched when a token names a key ID not seen before.
func TestOIDCKeyRotation(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{KeyRefreshInterval: time.Nanosecond})
	ready(t, a)
	if _, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil))); err != nil {
		t.Fatal(err)
	}
	is.Rotate("k2")
	if _, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil))); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
}

// Without OIDC configured, a token that is not an API token is simply not accepted.
func TestNoOIDCRejectsJWTs(t *testing.T) {
	is := oidctest.New(t)
	a, _, _ := newAuth(t, nil)
	if _, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil))); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

// Tokens anyone can forge must not make Opsward fetch the issuer's keys on every request:
// a known key ID with a bad signature never refetches, and unknown key IDs refetch at
// most once per refresh interval.
func TestOIDCForgedTokensDoNotRefetchKeys(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{})
	ready(t, a)
	if _, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil))); err != nil {
		t.Fatal(err)
	}
	before := is.KeyFetches.Load()
	forger := oidctest.Key(t)
	for i := range 20 {
		for _, kid := range []string{"k1", fmt.Sprintf("unknown-%d", i), ""} {
			if _, _, err := a.Authenticate(context.Background(), oidctest.SignWith(t, forger, kid, claims(is, nil))); err == nil {
				t.Fatalf("a forged token with kid %q was accepted", kid)
			}
		}
	}
	if got := is.KeyFetches.Load() - before; got > 1 {
		t.Fatalf("60 forged tokens caused %d key set fetches, want at most 1", got)
	}
}

// When the issuer's keys cannot be fetched for a new key ID, the token cannot be judged.
func TestOIDCKeysUnavailableIsNotARejection(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{KeyRefreshInterval: time.Nanosecond})
	ready(t, a)
	is.Rotate("k2")
	is.KeysDown.Store(true)
	_, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, nil)))
	if err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want an error that is not a rejection", err)
	}
}

// ID tokens and other token types are not access tokens, whatever their audience.
func TestOIDCRejectsNonAccessTokens(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{})
	ready(t, a)
	key, kid := is.Current()
	for name, tok := range map[string]string{
		"payload typ ID":  is.Sign(claims(is, map[string]any{"typ": "ID"})),
		"header typ dpop": oidctest.SignTyped(t, key, kid, "dpop+jwt", claims(is, nil)),
	} {
		if _, _, err := a.Authenticate(context.Background(), tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v, want a rejection", name, err)
		}
	}
	for _, typ := range []string{"at+jwt", "application/at+jwt", "JWT"} {
		if _, _, err := a.Authenticate(context.Background(), oidctest.SignTyped(t, key, kid, typ, claims(is, nil))); err != nil {
			t.Errorf("header typ %s: %v", typ, err)
		}
	}
}

// Email subjects are compared without case, so that a deny on alice@ also stops Alice@.
func TestOIDCEmailSubjectIsLowercased(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{})
	ready(t, a)
	p, _, err := a.Authenticate(context.Background(), is.Sign(claims(is, map[string]any{"email": "Alice@Example.COM"})))
	if err != nil || p.Subject != "alice@example.com" || !slices.Contains(p.Groups, "platform") {
		t.Fatalf("principal = %+v, %v; want the lowercased subject and its internal groups", p, err)
	}
}

// An issuer that names itself differently will not change by retrying: say so, loudly,
// with the value to configure.
func TestOIDCIssuerMismatchIsAnError(t *testing.T) {
	is := oidctest.New(t)
	is.Advertise = is.URL + "/"
	var logs strings.Builder
	a := New(Options{Store: openStore(t), Logger: slog.New(slog.NewJSONHandler(&logs, nil)), OIDC: &OIDCOptions{
		Issuer: is.URL, Audience: audience, SubjectClaim: "email", GroupsClaim: "groups", RetryInterval: time.Hour,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	a.Discover(ctx)
	out := logs.String()
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, is.URL+"/") || !strings.Contains(out, "identity.oidc.issuer") {
		t.Fatalf("log = %s", out)
	}
}
