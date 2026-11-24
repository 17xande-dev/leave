package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/17xande-dev/websec/session"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/users"
)

func newSessions(t *testing.T, d *sql.DB) *Sessions {
	t.Helper()
	s, err := NewSessions(d, session.Default)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	u, _ := us.Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := newSessions(t, d)

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
	s := newSessions(t, d)
	tok, _ := s.Create(ctx, a.ID)

	if _, _, err := us.Sync(ctx, []directory.User{{ID: "CN=b", DN: "CN=b", Username: "b", Name: "B"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("disabled user's session still valid: %v", err)
	}
}

// A session past the absolute limit is refused and swept, however recently it
// was used.
func TestExpiredSessionRefused(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	u, _ := users.NewStore(d).Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := newSessions(t, d)
	tok, _ := s.Create(ctx, u.ID)
	now := time.Now()
	backdate(t, d, tok, now.Add(-session.Default.Absolute-time.Minute), now.Add(-time.Minute))
	if _, err := s.Lookup(ctx, tok); err != ErrNoSession {
		t.Fatalf("expired session accepted: %v", err)
	}
	if n, _ := s.DeleteExpired(ctx); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
}

// A request must not extend a session: expires_at, the absolute limit, is
// written once at sign-in. (The old code moved it on every request.)
func TestLookupDoesNotMoveExpiry(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	u, _ := users.NewStore(d).Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := newSessions(t, d)
	tok, _ := s.Create(ctx, u.ID)
	now := time.Now()
	backdate(t, d, tok, now.Add(-time.Hour), now.Add(-30*time.Minute)) // old last_seen: the touch fires
	var before, after string
	d.QueryRow(`SELECT expires_at FROM sessions`).Scan(&before)
	if _, err := s.Lookup(ctx, tok); err != nil {
		t.Fatal(err)
	}
	d.QueryRow(`SELECT expires_at FROM sessions`).Scan(&after)
	if before != after {
		t.Fatalf("lookup moved expires_at: %s -> %s", before, after)
	}
}

// backdate rewrites a session's stored times as if it were created and last
// seen then, with expires_at as Create would have set it.
func backdate(t *testing.T, d *sql.DB, token string, created, lastSeen time.Time) {
	t.Helper()
	if _, err := d.Exec(`UPDATE sessions SET created_at = ?, last_seen = ?, expires_at = ? WHERE token_hash = ?`,
		stamp(created), stamp(lastSeen), stamp(session.Default.AbsoluteExpiry(created)), hash(token)); err != nil {
		t.Fatal(err)
	}
}

// A row written before last_seen existed (its sliding expires_at still in
// force, last_seen set to the migration time) keeps working, and its own
// expires_at still ends it.
func TestSessionFromBeforeTheMigrationSurvives(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	u, _ := users.NewStore(d).Upsert(ctx, directory.User{ID: "CN=a", DN: "CN=a", Username: "a", Name: "A"})
	s := newSessions(t, d)
	now := time.Now()
	for _, tc := range []struct {
		name    string
		expires time.Time
		ok      bool
	}{{"unexpired", now.Add(30 * time.Minute), true}, {"expired", now.Add(-time.Minute), false}} {
		tok, _ := s.Create(ctx, u.ID)
		if _, err := d.Exec(`UPDATE sessions SET created_at = ?, last_seen = ?, expires_at = ? WHERE token_hash = ?`,
			stamp(now.Add(-time.Hour)), stamp(now), stamp(tc.expires), hash(tok)); err != nil {
			t.Fatal(err)
		}
		_, err := s.Lookup(ctx, tok)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%t", tc.name, err, tc.ok)
		}
	}
}
