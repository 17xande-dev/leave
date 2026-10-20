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

func team(id, name string, members ...string) directory.Team {
	t := directory.Team{ID: id, Name: name}
	for _, m := range members {
		t.MemberIDs = append(t.MemberIDs, "guid-"+m)
	}
	return t
}

func members(t *testing.T, s *Store, team string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT u.username FROM team_members m JOIN teams x ON x.id = m.team_id
		JOIN users u ON u.id = m.user_id WHERE x.ext_id = ? ORDER BY u.username`, team)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func TestSyncTeams(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.New(t))
	if _, _, err := s.Sync(ctx, []directory.User{dirUser("a"), dirUser("b"), dirUser("c")}); err != nil {
		t.Fatal(err)
	}
	// "ghost" has no account here and is skipped.
	n, gone, err := s.SyncTeams(ctx, "fake", []directory.Team{team("t1", "Band", "a", "b", "ghost"), team("t2", "Choir", "c")})
	if err != nil || n != 2 || gone != 0 {
		t.Fatalf("first sync: %d %d %v", n, gone, err)
	}
	if got := members(t, s, "t1"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("members = %v", got)
	}
	var bandID int64
	s.db.QueryRow(`SELECT id FROM teams WHERE ext_id = 't1'`).Scan(&bandID)
	s.db.Exec(`INSERT INTO team_rules (team_id, max_percent) VALUES (?, 40)`, bandID)

	// Memberships are replaced, a renamed team keeps its row, and the team
	// missing from the listing is disabled, not deleted.
	n, gone, err = s.SyncTeams(ctx, "fake", []directory.Team{team("t1", "House band", "c")})
	if err != nil || n != 1 || gone != 1 {
		t.Fatalf("second sync: %d %d %v", n, gone, err)
	}
	if got := members(t, s, "t1"); len(got) != 1 || got[0] != "c" {
		t.Fatalf("members after = %v", got)
	}
	var name string
	var id int64
	var disabled bool
	s.db.QueryRow(`SELECT id, name FROM teams WHERE ext_id = 't1'`).Scan(&id, &name)
	if id != bandID || name != "House band" {
		t.Fatalf("renamed team = %d %q", id, name)
	}
	s.db.QueryRow(`SELECT disabled FROM teams WHERE ext_id = 't2'`).Scan(&disabled)
	if !disabled {
		t.Fatal("t2 was not disabled")
	}
	// Another source's teams are left alone by this one's sync.
	if _, _, err := s.SyncTeams(ctx, "pco", []directory.Team{team("p1", "Tech")}); err != nil {
		t.Fatal(err)
	}
	if _, gone, _ = s.SyncTeams(ctx, "fake", []directory.Team{team("t1", "House band", "c")}); gone != 0 {
		t.Fatalf("fake sync disabled another source's team")
	}
	// A returning team is re-enabled with its rule; an empty listing is applied.
	if _, _, err := s.SyncTeams(ctx, "fake", []directory.Team{team("t2", "Choir", "a")}); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT disabled FROM teams WHERE ext_id = 't2'`).Scan(&disabled)
	if disabled {
		t.Fatal("t2 not re-enabled")
	}
	if _, gone, _ = s.SyncTeams(ctx, "fake", nil); gone != 1 {
		t.Fatalf("empty listing disabled %d, want 1", gone)
	}
	var rule int
	if s.db.QueryRow(`SELECT max_percent FROM team_rules WHERE team_id = ?`, bandID).Scan(&rule); rule != 40 {
		t.Fatalf("rule lost: %d", rule)
	}
}
