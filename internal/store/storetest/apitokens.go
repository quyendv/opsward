// Package storetest holds behavior every store.Store implementation must show, so that
// each implementation's tests run the same cases.
package storetest

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func token(id string, created time.Time) store.APIToken {
	return store.APIToken{
		ID: id, SecretHash: bytes.Repeat([]byte{1}, 32), Subject: "ci@example.com", Groups: []string{"deployers", "team-a"},
		Description: "release pipeline", CreatedAt: created, ExpiresAt: created.Add(30 * 24 * time.Hour),
	}
}

// APITokens checks creating, reading, listing, revoking and touching API tokens.
func APITokens(t *testing.T, open func(t *testing.T) store.Store) {
	ctx := context.Background()

	t.Run("round trip", func(t *testing.T) {
		s := open(t)
		want := token("aaaaaaaaaaaa", t0)
		if err := s.CreateAPIToken(ctx, want); err != nil {
			t.Fatalf("CreateAPIToken: %v", err)
		}
		got, ok, err := s.APIToken(ctx, want.ID)
		if err != nil || !ok {
			t.Fatalf("APIToken = %v, %v", ok, err)
		}
		if got.ID != want.ID || !bytes.Equal(got.SecretHash, want.SecretHash) || got.Subject != want.Subject ||
			!slices.Equal(got.Groups, want.Groups) || got.Description != want.Description ||
			!got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) ||
			!got.RevokedAt.IsZero() || !got.LastUsedAt.IsZero() {
			t.Fatalf("APIToken = %+v, want %+v", got, want)
		}
	})

	t.Run("missing", func(t *testing.T) {
		s := open(t)
		if _, ok, err := s.APIToken(ctx, "nope"); err != nil || ok {
			t.Fatalf("APIToken(missing) = %v, %v", ok, err)
		}
		if ok, err := s.RevokeAPIToken(ctx, "nope", t0); err != nil || ok {
			t.Fatalf("RevokeAPIToken(missing) = %v, %v", ok, err)
		}
	})

	t.Run("duplicate id", func(t *testing.T) {
		s := open(t)
		if err := s.CreateAPIToken(ctx, token("aaaaaaaaaaaa", t0)); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateAPIToken(ctx, token("aaaaaaaaaaaa", t0)); err == nil {
			t.Fatal("a second token with the same id was accepted")
		}
	})

	t.Run("no groups", func(t *testing.T) {
		s := open(t)
		tok := token("aaaaaaaaaaaa", t0)
		tok.Groups = nil
		if err := s.CreateAPIToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.APIToken(ctx, tok.ID)
		if err != nil || len(got.Groups) != 0 {
			t.Fatalf("groups = %v, %v", got.Groups, err)
		}
	})

	t.Run("list by creation", func(t *testing.T) {
		s := open(t)
		for i, id := range []string{"bbbbbbbbbbbb", "aaaaaaaaaaaa", "cccccccccccc"} {
			if err := s.CreateAPIToken(ctx, token(id, t0.Add(time.Duration(i)*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		list, err := s.ListAPITokens(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, tok := range list {
			ids = append(ids, tok.ID)
		}
		if !slices.Equal(ids, []string{"bbbbbbbbbbbb", "aaaaaaaaaaaa", "cccccccccccc"}) {
			t.Fatalf("ListAPITokens order = %v", ids)
		}
	})

	t.Run("revoke keeps the first time", func(t *testing.T) {
		s := open(t)
		if err := s.CreateAPIToken(ctx, token("aaaaaaaaaaaa", t0)); err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
			if ok, err := s.RevokeAPIToken(ctx, "aaaaaaaaaaaa", at); err != nil || !ok {
				t.Fatalf("RevokeAPIToken = %v, %v", ok, err)
			}
		}
		got, _, _ := s.APIToken(ctx, "aaaaaaaaaaaa")
		if !got.RevokedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("RevokedAt = %v, want the first revocation", got.RevokedAt)
		}
	})

	t.Run("touch", func(t *testing.T) {
		s := open(t)
		if err := s.CreateAPIToken(ctx, token("aaaaaaaaaaaa", t0)); err != nil {
			t.Fatal(err)
		}
		at := t0.Add(90 * time.Minute)
		if err := s.TouchAPIToken(ctx, "aaaaaaaaaaaa", at); err != nil {
			t.Fatal(err)
		}
		got, _, _ := s.APIToken(ctx, "aaaaaaaaaaaa")
		if !got.LastUsedAt.Equal(at) {
			t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, at)
		}
	})
}
