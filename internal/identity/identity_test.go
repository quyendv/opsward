package identity

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
	"github.com/quyendv/opsward/internal/store/sqlite"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func openStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "opsward.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newAuth(t *testing.T, groups map[string][]string) (*Authenticator, store.Store, *clock) {
	t.Helper()
	st := openStore(t)
	c := &clock{now: t0}
	a := New(Options{Store: st, Groups: groups, Now: c.Now})
	return a, st, c
}

func create(t *testing.T, a *Authenticator, subject string, groups []string) (string, store.APIToken) {
	t.Helper()
	tok, rec, err := a.CreateToken(context.Background(), TokenRequest{Subject: subject, Groups: groups, Description: "test", TTL: time.Hour, MaxTTL: 365 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok, rec
}

func TestTokenFormat(t *testing.T) {
	a, st, _ := newAuth(t, nil)
	tok, rec := create(t, a, "ci@example.com", nil)
	if !strings.HasPrefix(tok, "opw_") || len(rec.ID) != 12 || !strings.HasPrefix(tok, "opw_"+rec.ID+"_") {
		t.Fatalf("token %q, id %q", tok, rec.ID)
	}
	if secret := strings.TrimPrefix(tok, "opw_"+rec.ID+"_"); len(secret) < 43 {
		t.Fatalf("secret %q is shorter than 256 bits", secret)
	}
	stored, _, _ := st.APIToken(context.Background(), rec.ID)
	if strings.Contains(string(stored.SecretHash), strings.TrimPrefix(tok, "opw_"+rec.ID+"_")) {
		t.Fatal("the store holds the secret")
	}
	tok2, _ := create(t, a, "ci@example.com", nil)
	if tok2 == tok {
		t.Fatal("two tokens are equal")
	}
}

func TestAuthenticateToken(t *testing.T) {
	a, _, _ := newAuth(t, map[string][]string{"platform": {"ci@example.com"}, "other": {"bob@example.com"}})
	tok, rec := create(t, a, "ci@example.com", []string{"deployers", "platform"})
	p, exp, err := a.Authenticate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Subject != "ci@example.com" || p.Credential != "token:"+rec.ID {
		t.Fatalf("principal = %+v", p)
	}
	if !slices.Equal(p.Groups, []string{"deployers", "platform"}) {
		t.Fatalf("groups = %v, want token and internal groups, sorted, once each", p.Groups)
	}
	if !exp.Equal(t0.Add(time.Hour)) {
		t.Fatalf("expiry = %v", exp)
	}
}

func TestAuthenticateRejects(t *testing.T) {
	a, st, c := newAuth(t, nil)
	ctx := context.Background()
	good, rec := create(t, a, "ci@example.com", nil)
	revokedTok, revoked := create(t, a, "ci@example.com", nil)
	if _, err := st.RevokeAPIToken(ctx, revoked.ID, t0); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		token  string
		reason string
	}{
		"wrong secret":   {good[:len(good)-4] + "aaaa", "secret does not match"},
		"unknown id":     {"opw_zzzzzzzzzzzz_" + strings.TrimPrefix(good, "opw_"+rec.ID+"_"), "unknown token"},
		"malformed":      {"opw_short", "malformed"},
		"no prefix":      {"eyJhbGciOiJSUzI1NiJ9.e30.sig", "not an API token"},
		"revoked":        {revokedTok, "revoked"},
		"empty":          {"", "not an API token"},
		"id with dashes": {"opw_------------_" + strings.Repeat("a", 52), "malformed"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := a.Authenticate(ctx, tt.token)
			if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("err = %v, want ErrUnauthenticated with %q", err, tt.reason)
			}
		})
	}
	c.now = t0.Add(time.Hour)
	if _, _, err := a.Authenticate(ctx, good); !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("at expiry: err = %v", err)
	}
}

func TestAuthenticateRecordsLastUseAtMostOncePerMinute(t *testing.T) {
	a, st, c := newAuth(t, nil)
	ctx := context.Background()
	tok, rec := create(t, a, "ci@example.com", nil)
	used := func() time.Time {
		got, _, _ := st.APIToken(ctx, rec.ID)
		return got.LastUsedAt
	}
	if _, _, err := a.Authenticate(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if !used().Equal(t0) {
		t.Fatalf("first use not recorded: %v", used())
	}
	c.now = t0.Add(30 * time.Second)
	_, _, _ = a.Authenticate(ctx, tok)
	if !used().Equal(t0) {
		t.Fatalf("recorded again within a minute: %v", used())
	}
	c.now = t0.Add(61 * time.Second)
	_, _, _ = a.Authenticate(ctx, tok)
	if !used().Equal(c.now) {
		t.Fatalf("not recorded after a minute: %v", used())
	}
}

func TestSetGroupsAppliesToTheNextRequest(t *testing.T) {
	a, _, _ := newAuth(t, nil)
	tok, _ := create(t, a, "alice@example.com", nil)
	a.SetGroups(map[string][]string{"platform": {"alice@example.com"}})
	p, _, err := a.Authenticate(context.Background(), tok)
	if err != nil || !slices.Equal(p.Groups, []string{"platform"}) {
		t.Fatalf("groups = %v, %v", p.Groups, err)
	}
	a.SetGroups(nil)
	p, _, _ = a.Authenticate(context.Background(), tok)
	if len(p.Groups) != 0 {
		t.Fatalf("groups after removal = %v", p.Groups)
	}
}

func TestCreateTokenRejects(t *testing.T) {
	a, _, _ := newAuth(t, nil)
	for name, tt := range map[string]struct {
		req  TokenRequest
		want string
	}{
		"no subject":     {TokenRequest{TTL: time.Hour, MaxTTL: 24 * time.Hour}, "subject is required"},
		"zero ttl":       {TokenRequest{Subject: "a", MaxTTL: 24 * time.Hour}, "ttl must be positive"},
		"over max":       {TokenRequest{Subject: "a", TTL: 48 * time.Hour, MaxTTL: 24 * time.Hour}, "longer than identity.apiTokens.maxTTL"},
		"empty group":    {TokenRequest{Subject: "a", Groups: []string{""}, TTL: time.Hour, MaxTTL: 24 * time.Hour}, "empty group"},
		"spaces subject": {TokenRequest{Subject: " a", TTL: time.Hour, MaxTTL: 24 * time.Hour}, "leading or trailing space"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := a.CreateToken(context.Background(), tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
