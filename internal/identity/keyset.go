package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	defaultKeyRefreshInterval = 30 * time.Second
	maxKeySetBytes            = 1 << 20
)

// keySet verifies token signatures with the issuer's published keys. Unlike go-oidc's
// remote key set, which fetches the keys again after any failed verification, it fetches
// them only for a key ID it does not hold, and at most once per refresh interval: a
// token anyone can forge must not make Opsward call the issuer on every request.
type keySet struct {
	url        string
	client     *http.Client
	algs       []jose.SignatureAlgorithm
	minRefresh time.Duration
	now        func() time.Time

	mu      sync.Mutex
	keys    []jose.JSONWebKey
	fetched time.Time // last attempt, successful or not
}

// VerifySignature implements oidc.KeySet.
func (k *keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, k.algs)
	if err != nil {
		return nil, fmt.Errorf("malformed token: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("token must have exactly one signature")
	}
	kid := jws.Signatures[0].Header.KeyID
	if payload, tried, err := k.verify(jws, kid); tried {
		return payload, err
	}
	if err := k.refresh(ctx); err != nil {
		return nil, err
	}
	payload, tried, err := k.verify(jws, kid)
	if !tried {
		return nil, fmt.Errorf("no published key has ID %q", kid)
	}
	return payload, err
}

// verify tries the held keys that match kid (every key when the token names none). It
// reports whether any key was tried.
func (k *keySet) verify(jws *jose.JSONWebSignature, kid string) ([]byte, bool, error) {
	k.mu.Lock()
	keys := k.keys
	k.mu.Unlock()
	tried := false
	for _, key := range keys {
		if kid != "" && key.KeyID != kid {
			continue
		}
		tried = true
		if payload, err := jws.Verify(key); err == nil {
			return payload, true, nil
		}
	}
	return nil, tried, errors.New("signature does not verify")
}

func (k *keySet) refresh(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.fetched.IsZero() && k.now().Sub(k.fetched) < k.minRefresh {
		return nil
	}
	k.fetched = k.now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("issuer keys: %w", err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("issuer keys: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("issuer keys: HTTP %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeySetBytes)).Decode(&set); err != nil {
		return fmt.Errorf("issuer keys: %w", err)
	}
	var keys []jose.JSONWebKey
	for _, key := range set.Keys {
		if key.Use == "" || key.Use == "sig" {
			keys = append(keys, key)
		}
	}
	k.keys = keys
	return nil
}
