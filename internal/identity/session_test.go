package identity

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/identity/oidctest"
)

// A console session started with an API token stays only as valid as the token, and
// takes its groups from it.
func TestPrincipalForATokenSession(t *testing.T) {
	a, st, c := newAuth(t, map[string][]string{"platform": {"ci@example.com"}})
	ctx := context.Background()
	_, rec := create(t, a, "ci@example.com", []string{"deployers"})
	id := Identity{Subject: "ci@example.com", Credential: "token:" + rec.ID}
	p, err := a.Principal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "ci@example.com" || p.Credential != "token:"+rec.ID || !slices.Equal(p.Groups, []string{"deployers", "platform"}) {
		t.Fatalf("principal = %+v", p)
	}
	// Internal groups are the current ones.
	a.SetGroups(nil)
	if p, _ := a.Principal(ctx, id); !slices.Equal(p.Groups, []string{"deployers"}) {
		t.Fatalf("groups after the reload = %v", p.Groups)
	}
	c.now = t0.Add(time.Hour)
	if _, err := a.Principal(ctx, id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an expired token: err = %v", err)
	}
	c.now = t0
	if _, err := st.RevokeAPIToken(ctx, rec.ID, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Principal(ctx, id); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a revoked token: err = %v", err)
	}
	if _, err := a.Principal(ctx, Identity{Subject: "other@example.com", Credential: "token:" + rec.ID}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a token of another subject: err = %v", err)
	}
	if _, err := a.Principal(ctx, Identity{Subject: "ci@example.com", Credential: "token:nope"}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("an unknown token: err = %v", err)
	}
}

func TestPrincipalForAnOIDCSession(t *testing.T) {
	a, _, _ := newAuth(t, map[string][]string{"platform": {"alice@example.com"}})
	p, err := a.Principal(context.Background(), Identity{Subject: "Alice@Example.com", Groups: []string{"oidc:team-a"}, Credential: "oidc"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "alice@example.com" || p.Credential != "oidc" || !slices.Equal(p.Groups, []string{"oidc:team-a", "platform"}) {
		t.Fatalf("principal = %+v", p)
	}
}

const consoleClient = "opsward-console"

func idToken(is *oidctest.Issuer, over map[string]any) string {
	c := is.Claims(consoleClient, map[string]any{"nonce": "n-1"})
	for k, v := range over {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return is.Sign(c)
}

// An ID token is mapped as an access token is, so a person is the same principal in
// the console as through their agent.
func TestVerifyIDToken(t *testing.T) {
	is := oidctest.New(t)
	a := oidcAuth(t, is, OIDCOptions{GroupsPrefix: "oidc:"})
	ready(t, a)
	ctx := context.Background()
	id, err := a.VerifyIDToken(ctx, idToken(is, nil), consoleClient, "n-1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "alice@example.com" || id.Credential != "oidc" || !slices.Equal(id.Groups, []string{"oidc:team-a", "oidc:admins"}) ||
		id.Expires.IsZero() {
		t.Fatalf("identity = %+v: groups prefixed, internal groups not added", id)
	}
	for name, raw := range map[string]string{
		"another audience": is.Sign(is.Claims(audience, map[string]any{"nonce": "n-1"})),
		"another nonce":    idToken(is, map[string]any{"nonce": "n-2"}),
		"no nonce":         idToken(is, map[string]any{"nonce": nil}),
		"unverified email": idToken(is, map[string]any{"email_verified": false}),
		"no subject claim": idToken(is, map[string]any{"email": nil}),
		"expired":          idToken(is, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
		"another issuer's": oidctest.SignWith(t, oidctest.Key(t), "k1", is.Claims(consoleClient, map[string]any{"nonce": "n-1"})),
	} {
		if _, err := a.VerifyIDToken(ctx, raw, consoleClient, "n-1"); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if auth, token, ok := a.Endpoints(); !ok || auth != is.URL+"/auth" || token != is.URL+"/token" {
		t.Fatalf("endpoints = %q %q %v", auth, token, ok)
	}
}

func TestVerifyIDTokenBeforeDiscovery(t *testing.T) {
	a, _, _ := newAuth(t, nil)
	if _, err := a.VerifyIDToken(context.Background(), "x.y.z", consoleClient, "n"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v, want an error that is not a rejection", err)
	}
	if _, _, ok := a.Endpoints(); ok {
		t.Fatal("endpoints without OIDC")
	}
}

// Using the console with an API token counts as using the token.
func TestPrincipalForATokenSessionRecordsUse(t *testing.T) {
	a, st, c := newAuth(t, nil)
	ctx := context.Background()
	_, rec := create(t, a, "ci@example.com", nil)
	c.now = t0.Add(10 * time.Minute)
	if _, err := a.Principal(ctx, Identity{Subject: "ci@example.com", Credential: "token:" + rec.ID}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.APIToken(ctx, rec.ID); !got.LastUsedAt.Equal(c.now) {
		t.Fatalf("last used %v, want %v", got.LastUsedAt, c.now)
	}
}
