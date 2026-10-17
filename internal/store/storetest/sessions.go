package storetest

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/quyendv/opsward/internal/store"
)

// Sessions checks console session storage.
func Sessions(t *testing.T, open func(t *testing.T) store.Store) {
	ctx := context.Background()
	session := func(id string, expires time.Time) store.Session {
		return store.Session{
			IDHash: []byte("hash-" + id), Subject: "alice@example.com", Groups: []string{"team-a", "platform"},
			Credential: "oidc", CreatedAt: t0, ExpiresAt: expires,
		}
	}

	t.Run("session create, read and delete", func(t *testing.T) {
		s := open(t)
		want := session("1", t0.Add(8*time.Hour))
		if err := s.CreateSession(ctx, want); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(ctx, want); err == nil {
			t.Fatal("CreateSession accepted an existing identifier")
		}
		got, ok, err := s.Session(ctx, want.IDHash)
		if err != nil || !ok || !bytes.Equal(got.IDHash, want.IDHash) || got.Subject != want.Subject ||
			!slices.Equal(got.Groups, want.Groups) || got.Credential != want.Credential ||
			!got.CreatedAt.Equal(want.CreatedAt) || !got.ExpiresAt.Equal(want.ExpiresAt) {
			t.Fatalf("Session = %+v, %v, %v", got, ok, err)
		}
		if err := s.DeleteSession(ctx, want.IDHash); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := s.Session(ctx, want.IDHash); err != nil || ok {
			t.Fatalf("a deleted session was found: %v, %v", ok, err)
		}
		if err := s.DeleteSession(ctx, want.IDHash); err != nil {
			t.Fatalf("deleting a missing session: %v", err)
		}
	})

	t.Run("session without groups", func(t *testing.T) {
		s := open(t)
		want := session("1", t0.Add(time.Hour))
		want.Groups = nil
		if err := s.CreateSession(ctx, want); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := s.Session(ctx, want.IDHash); len(got.Groups) != 0 {
			t.Fatalf("groups = %q", got.Groups)
		}
	})

	t.Run("session prune", func(t *testing.T) {
		s := open(t)
		for i, h := range []time.Duration{1, 2, 3} {
			if err := s.CreateSession(ctx, session(string(rune('a'+i)), t0.Add(h*time.Hour))); err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.PruneSessions(ctx, t0.Add(2*time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("PruneSessions = %d, %v; want the one expired before", n, err)
		}
		if _, ok, _ := s.Session(ctx, []byte("hash-b")); !ok {
			t.Fatal("a session expiring at the cut was pruned")
		}
	})
}
