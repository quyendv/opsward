package identity

import (
	"context"
	"errors"
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
	if _, _, err := a.Authenticate(context.Background(), tok); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "not discovered") {
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
	a := oidcAuth(t, is, OIDCOptions{})
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
