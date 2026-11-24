package auth

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/websec/session"
	"github.com/17xande-dev/websec/session/sessiontest"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/users"
)

// TestSessionStoreConformance runs websec's store suite against the real store,
// the real Lifetime and a real (temporary) database.
func TestSessionStoreConformance(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	s := newSessions(t, d)
	var n int
	uid := func(user string) int64 {
		id, err := strconv.ParseInt(user, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sessiontest.Run(t, sessiontest.Harness{
		Lifetime: session.Default,
		NewUser: func(t *testing.T) string {
			n++
			u, err := us.Upsert(ctx, directory.User{ID: fmt.Sprintf("CN=u%d", n), DN: fmt.Sprintf("CN=u%d", n),
				Username: fmt.Sprintf("u%d", n), Name: fmt.Sprintf("User %d", n)})
			if err != nil {
				t.Fatal(err)
			}
			return strconv.FormatInt(u.ID, 10)
		},
		Create: func(t *testing.T, user string) string {
			tok, err := s.Create(ctx, uid(user))
			if err != nil {
				t.Fatal(err)
			}
			return tok
		},
		Lookup: func(t *testing.T, token string) (string, bool, error) {
			u, err := s.Lookup(ctx, token)
			if err == ErrNoSession {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
			return strconv.FormatInt(u.ID, 10), true, nil
		},
		Backdate: func(t *testing.T, token string, created, lastSeen time.Time) {
			backdate(t, d, token, created, lastSeen)
		},
		LastSeen: func(t *testing.T, token string) time.Time {
			var v string
			if err := d.QueryRow(`SELECT last_seen FROM sessions WHERE token_hash = ?`, hash(token)).Scan(&v); err != nil {
				t.Fatal(err)
			}
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				t.Fatal(err)
			}
			return ts
		},
		Sweep: func(t *testing.T) int {
			removed, err := s.DeleteExpired(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return int(removed)
		},
		Count: func(t *testing.T) int {
			var c int
			if err := d.QueryRow(`SELECT count(*) FROM sessions`).Scan(&c); err != nil {
				t.Fatal(err)
			}
			return c
		},
		RawRows: func(t *testing.T) []string {
			rows, err := d.Query(`SELECT token_hash, user_id, created_at, last_seen, expires_at FROM sessions`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var h, c, l, e string
				var u int64
				if err := rows.Scan(&h, &u, &c, &l, &e); err != nil {
					t.Fatal(err)
				}
				out = append(out, strings.Join([]string{h, strconv.FormatInt(u, 10), c, l, e}, "|"))
			}
			return out
		},
		// Disabled in the users table: what Sync does to someone the directory
		// stopped listing, and what the lookup's join checks on every request.
		Disable: func(t *testing.T, user string) {
			if _, err := d.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, uid(user)); err != nil {
				t.Fatal(err)
			}
		},
		// Directory revocation through the real path: a sync whose listing no
		// longer has the person.
		RevokeUser: func(t *testing.T, user string) {
			rows, err := d.Query(`SELECT ext_id, dn, username, name FROM users WHERE id <> ? AND disabled = 0`, uid(user))
			if err != nil {
				t.Fatal(err)
			}
			var rest []directory.User
			for rows.Next() {
				var u directory.User
				if err := rows.Scan(&u.ID, &u.DN, &u.Username, &u.Name); err != nil {
					t.Fatal(err)
				}
				rest = append(rest, u)
			}
			rows.Close()
			if _, _, err := us.Sync(ctx, rest); err != nil {
				t.Fatal(err)
			}
		},
		NotApplicable: map[sessiontest.Case]string{
			sessiontest.CasePasswordChange: "directory password: the app has no local password to change",
		},
	})
}
