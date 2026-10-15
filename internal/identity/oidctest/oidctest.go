// Package oidctest is a fake OpenID Connect issuer for tests: discovery, a key set and
// token signing.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Issuer serves discovery and keys at URL.
type Issuer struct {
	URL  string
	Down atomic.Bool // discovery fails while set

	t   *testing.T
	mu  sync.Mutex
	key *rsa.PrivateKey
	kid string
}

// New starts an issuer that is stopped when the test ends.
func New(t *testing.T) *Issuer {
	t.Helper()
	is := &Issuer{t: t, key: Key(t), kid: "k1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if is.Down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.URL, "jwks_uri": is.URL + "/keys",
			"authorization_endpoint": is.URL + "/auth", "token_endpoint": is.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		is.mu.Lock()
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &is.key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig"}}}
		is.mu.Unlock()
		_ = json.NewEncoder(w).Encode(set)
	})
	srv := httptest.NewServer(mux)
	is.URL = srv.URL
	t.Cleanup(srv.Close)
	return is
}

// Key generates an RSA key.
func Key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Rotate replaces the signing key and its key ID.
func (is *Issuer) Rotate(kid string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.key, is.kid = Key(is.t), kid
}

// Claims are a valid token's claims for audience, with over applied: a nil value
// removes a claim.
func (is *Issuer) Claims(audience string, over map[string]any) map[string]any {
	c := map[string]any{
		"iss": is.URL, "aud": audience, "sub": "u-1", "email": "alice@example.com", "email_verified": true,
		"groups": []string{"team-a", "admins"}, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	}
	for k, v := range over {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

// Sign signs claims with the issuer's current key.
func (is *Issuer) Sign(claims map[string]any) string {
	is.mu.Lock()
	key, kid := is.key, is.kid
	is.mu.Unlock()
	return SignWith(is.t, key, kid, claims)
}

// SignWith signs claims with any key, to forge tokens.
func SignWith(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
