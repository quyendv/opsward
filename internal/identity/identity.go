// Package identity turns bearer tokens into principals: API tokens held in the store,
// and the internal groups a subject belongs to. docs/design/0005.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/store"
)

// TokenPrefix starts every API token, so that secret scanners can recognize one.
const TokenPrefix = "opw_"

const (
	idLength = 12
	// touchInterval bounds how often a token's last use is written.
	touchInterval = time.Minute
	// base32 is the alphabet of IDs and secrets: lowercase crypto/rand.Text.
	base32 = "abcdefghijklmnopqrstuvwxyz234567"
)

// ErrUnauthenticated is returned for a token that does not authenticate anyone. Its
// message says why, for the log; callers must not echo it to the client.
var ErrUnauthenticated = errors.New("unauthenticated")

func unauthenticated(reason string) error { return fmt.Errorf("%w: %s", ErrUnauthenticated, reason) }

// Options configures an Authenticator.
type Options struct {
	Store  store.Store
	Groups map[string][]string // internal groups: group name to member subjects
	Now    func() time.Time    // default time.Now
	Logger *slog.Logger
}

// Authenticator authenticates bearer tokens. It is safe for concurrent use.
type Authenticator struct {
	store store.Store
	now   func() time.Time
	log   *slog.Logger

	mu       sync.RWMutex
	memberOf map[string][]string // subject to internal groups
}

// New builds an Authenticator.
func New(opt Options) *Authenticator {
	a := &Authenticator{store: opt.Store, now: opt.Now, log: opt.Logger}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	a.SetGroups(opt.Groups)
	return a
}

// SetGroups replaces the internal groups, as on a configuration reload.
func (a *Authenticator) SetGroups(groups map[string][]string) {
	m := map[string][]string{}
	for g, members := range groups {
		for _, s := range members {
			m[s] = append(m[s], g)
		}
	}
	a.mu.Lock()
	a.memberOf = m
	a.mu.Unlock()
}

// Authenticate returns the principal a bearer token names, without an adapter, and the
// time its credential expires.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (core.Principal, time.Time, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return core.Principal{}, time.Time{}, unauthenticated("not an API token")
	}
	id, secret, ok := parse(token)
	if !ok {
		return core.Principal{}, time.Time{}, unauthenticated("malformed API token")
	}
	t, found, err := a.store.APIToken(ctx, id)
	if err != nil {
		return core.Principal{}, time.Time{}, err
	}
	// The hash is compared even for an unknown ID, so that timing does not tell the two apart.
	want := make([]byte, sha256.Size)
	if found {
		want = t.SecretHash
	}
	match := subtle.ConstantTimeCompare(hashSecret(secret), want) == 1
	now := a.now()
	switch {
	case !found:
		return core.Principal{}, time.Time{}, unauthenticated("unknown token " + id)
	case !match:
		return core.Principal{}, time.Time{}, unauthenticated("secret does not match for token " + id)
	case !t.RevokedAt.IsZero():
		return core.Principal{}, time.Time{}, unauthenticated("token " + id + " is revoked")
	case !now.Before(t.ExpiresAt):
		return core.Principal{}, time.Time{}, unauthenticated("token " + id + " expired")
	}
	if now.Sub(t.LastUsedAt) >= touchInterval {
		if err := a.store.TouchAPIToken(ctx, id, now); err != nil {
			a.log.Warn("recording API token use failed", "token", id, "error", err)
		}
	}
	return core.Principal{Subject: t.Subject, Groups: a.groups(t.Subject, t.Groups), Credential: "token:" + id}, t.ExpiresAt, nil
}

// groups is the union of a credential's groups and the subject's internal groups.
func (a *Authenticator) groups(subject string, fromCredential []string) []string {
	a.mu.RLock()
	internal := a.memberOf[subject]
	a.mu.RUnlock()
	out := slices.Concat(fromCredential, internal)
	slices.Sort(out)
	return slices.Compact(out)
}

// TokenRequest describes a token to create.
type TokenRequest struct {
	Subject     string
	Groups      []string
	Description string
	TTL         time.Duration
	MaxTTL      time.Duration
}

// CreateToken stores a new API token and returns it. The token is not kept anywhere:
// this is the only time it exists outside the caller.
func (a *Authenticator) CreateToken(ctx context.Context, req TokenRequest) (string, store.APIToken, error) {
	token, t, err := a.NewToken(req)
	if err != nil {
		return "", store.APIToken{}, err
	}
	if err := a.store.CreateAPIToken(ctx, t); err != nil {
		return "", store.APIToken{}, err
	}
	return token, t, nil
}

// NewToken validates req and generates a token and its stored form, without storing it.
func (a *Authenticator) NewToken(req TokenRequest) (string, store.APIToken, error) {
	switch {
	case req.Subject == "":
		return "", store.APIToken{}, errors.New("subject is required")
	case strings.TrimSpace(req.Subject) != req.Subject:
		return "", store.APIToken{}, errors.New("subject has leading or trailing space")
	case req.TTL <= 0:
		return "", store.APIToken{}, errors.New("ttl must be positive")
	case req.TTL > req.MaxTTL:
		return "", store.APIToken{}, fmt.Errorf("ttl %s is longer than identity.apiTokens.maxTTL (%s)", req.TTL, req.MaxTTL)
	case slices.Contains(req.Groups, ""):
		return "", store.APIToken{}, errors.New("empty group name")
	}
	id := strings.ToLower(rand.Text())[:idLength]
	secret := strings.ToLower(rand.Text() + rand.Text()) // 2 × 128 bits
	now := a.now().UTC()
	groups := slices.Clone(req.Groups)
	slices.Sort(groups)
	t := store.APIToken{
		ID: id, SecretHash: hashSecret(secret), Subject: req.Subject, Groups: slices.Compact(groups),
		Description: req.Description, CreatedAt: now, ExpiresAt: now.Add(req.TTL),
	}
	return TokenPrefix + id + "_" + secret, t, nil
}

// parse splits opw_<id>_<secret>. The ID has a fixed length, so the split does not
// depend on the secret's alphabet.
func parse(token string) (id, secret string, ok bool) {
	rest := strings.TrimPrefix(token, TokenPrefix)
	if len(rest) < idLength+2 || rest[idLength] != '_' {
		return "", "", false
	}
	id, secret = rest[:idLength], rest[idLength+1:]
	if !inAlphabet(id) || !inAlphabet(secret) {
		return "", "", false
	}
	return id, secret, true
}

func inAlphabet(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(base32, r) {
			return false
		}
	}
	return true
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}
