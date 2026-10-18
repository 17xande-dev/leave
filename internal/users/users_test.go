package users

import (
	"context"
	"testing"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
)

func dirUser(name string) directory.User {
	return directory.User{ID: "guid-" + name, DN: "CN=" + name + ",OU=Staff", Username: name, Name: name, Email: name + "@example.test"}
}

func TestSyncDisablesTheMissingAndRevivesTheReturning(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))

	if _, _, err := s.Sync(ctx, []directory.User{dirUser("a"), dirUser("b")}); err != nil {
		t.Fatal(err)
	}
	_, disabled, err := s.Sync(ctx, []directory.User{dirUser("a")})
	if err != nil || disabled != 1 {
		t.Fatalf("disabled=%d err=%v, want 1", disabled, err)
	}
	b, err := s.ByUsername(ctx, "b")
	if err != nil || !b.Disabled {
		t.Fatalf("b = %+v, %v; want disabled, not deleted", b, err)
	}
	// Re-enabled in AD: the same row comes back, so their history is intact.
	if _, _, err := s.Sync(ctx, []directory.User{dirUser("a"), dirUser("b")}); err != nil {
		t.Fatal(err)
	}
	b2, _ := s.ByUsername(ctx, "b")
	if b2.Disabled || b2.ID != b.ID {
		t.Fatalf("b after return = %+v, want enabled with id %d", b2, b.ID)
	}
}

// A broken base DN returns nobody. Applying that would disable every account,
// every admin included, and lock everyone out in one pass.
func TestSyncRefusesEmptyListing(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	s.Sync(ctx, []directory.User{dirUser("a")})
	if _, _, err := s.Sync(ctx, nil); err == nil {
		t.Fatal("empty sync accepted")
	}
	if a, _ := s.ByUsername(ctx, "a"); a.Disabled {
		t.Fatal("a was disabled by a refused sync")
	}
}

// AD compares DNs case-insensitively; a manager attribute that differs in case
// from the manager's own DN must still resolve.
func TestByDNIgnoresCase(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	s.Upsert(ctx, dirUser("boss"))
	if _, err := s.ByDN(ctx, "cn=BOSS,ou=staff"); err != nil {
		t.Fatalf("ByDN case-insensitive: %v", err)
	}
}

// Moving someone to another OU changes their DN. They must stay the same
// person, with the same row and history, and still be able to sign in.
func TestOUMoveKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	a, _ := s.Upsert(ctx, dirUser("sam"))
	moved := dirUser("sam")
	moved.DN = "CN=sam,OU=Kids,OU=Staff"
	b, err := s.Upsert(ctx, moved)
	if err != nil || b.ID != a.ID || b.DN != moved.DN {
		t.Fatalf("after OU move: %+v, %v (was id %d)", b, err, a.ID)
	}
}

// A leaver's username reused for a new starter is a new person. The leaver's
// row keeps its history under a retired name.
func TestUsernameReuse(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	old, _ := s.Upsert(ctx, dirUser("sam"))
	s.Sync(ctx, []directory.User{dirUser("other")}) // sam leaves
	newSam := dirUser("sam")
	newSam.ID = "guid-sam-2"
	n, err := s.Upsert(ctx, newSam)
	if err != nil || n.ID == old.ID {
		t.Fatalf("new sam = %+v, %v", n, err)
	}
	was, _ := s.ByID(ctx, old.ID)
	if was.Username == "sam" || !was.Disabled {
		t.Fatalf("old sam = %+v", was)
	}
	if got, _ := s.ByUsername(ctx, "sam"); got.ID != n.ID {
		t.Fatalf("ByUsername(sam) = %d, want the new starter %d", got.ID, n.ID)
	}
}

// The long-weekend flag is an admin's decision; a directory sync must not
// reset it.
func TestLongWeekendSurvivesSync(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	u, _ := s.Upsert(ctx, dirUser("sam"))
	if err := s.SetLongWeekend(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	s.Sync(ctx, []directory.User{dirUser("sam")})
	if got, _ := s.ByID(ctx, u.ID); !got.LongWeekend {
		t.Fatal("sync reset long_weekend")
	}
}
