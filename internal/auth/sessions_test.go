package auth

import (
	"context"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/users"
)

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	u, _ := us.Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := NewSessions(d, time.Hour)

	tok, err := s.Create(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Lookup(ctx, tok); err != nil || got.ID != u.ID {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	// The table holds a hash, not the token, so a leaked backup is not a
	// pile of live cookies.
	var n int
	d.QueryRow(`SELECT count(*) FROM sessions WHERE token_hash = ?`, tok).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored in the sessions table")
	}
	if err := s.Delete(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("lookup after delete: %v", err)
	}
}

// Disabling someone in AD reaches the app through the sync; their open
// session must stop working on the next request, not when it expires.
func TestDisabledUserHasNoSession(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	a, _ := us.Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	us.Upsert(ctx, directory.User{ID: "CN=b", DN: "CN=b", Username: "b", Name: "B"})
	s := NewSessions(d, time.Hour)
	tok, _ := s.Create(ctx, a.ID)

	if _, _, err := us.Sync(ctx, []directory.User{{ID: "CN=b", DN: "CN=b", Username: "b", Name: "B"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("disabled user's session still valid: %v", err)
	}
}

func TestExpiredSessionRefused(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	u, _ := users.NewStore(d).Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := NewSessions(d, -time.Minute)
	tok, _ := s.Create(ctx, u.ID)
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("expired session accepted: %v", err)
	}
	if n, _ := s.DeleteExpired(ctx); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}
