package directory

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestNormalizeUsername(t *testing.T) {
	for in, want := range map[string]string{
		"alex":             "alex",
		" Alex ":           "alex",
		`EXAMPLE\Alex`:     "alex",
		"alex@example.org": "alex",
	} {
		if got := NormalizeUsername(in); got != want {
			t.Errorf("NormalizeUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

// An empty password must never authenticate: against AD it is an anonymous
// bind that reports success.
func TestEmptyPasswordRefused(t *testing.T) {
	f := &Fake{}
	f.Add(User{Username: "alex", Name: "Alex"})
	if _, err := f.Authenticate(context.Background(), "alex", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("fake: empty password gave %v", err)
	}
	l, err := NewLDAP(LDAPConfig{URL: "ldaps://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	// Refused before any network I/O -- port 1 would otherwise produce a dial error.
	if _, err := l.Authenticate(context.Background(), "alex", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ldap: empty password gave %v", err)
	}
}

func TestNewLDAPRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "dc1:636", "http://dc1", "ldaps://"} {
		if _, err := NewLDAP(LDAPConfig{URL: u}); err == nil {
			t.Errorf("NewLDAP(%q) accepted", u)
		}
	}
}

func TestLoadFakeResolvesManagers(t *testing.T) {
	f, err := LoadFake("../../testdata/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	users, _ := f.AllUsers(context.Background())
	if len(users) < 4 {
		t.Fatalf("want at least 4 users in the fixture, got %d", len(users))
	}
	byDN := map[string]User{}
	for _, u := range users {
		byDN[u.DN] = u
	}
	for _, u := range users {
		if u.ManagerDN != "" {
			if _, ok := byDN[u.ManagerDN]; !ok {
				t.Errorf("%s's manager %s is not in the fixture", u.Username, u.ManagerDN)
			}
		}
	}
}

func TestFakeTeamsFromTheFixture(t *testing.T) {
	f, err := LoadFake("../../testdata/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	teams, err := f.Teams(context.Background())
	if err != nil || len(teams) != 2 || teams[0].Name != "Band" || teams[1].Name != "Kids team" {
		t.Fatalf("teams = %+v, %v", teams, err)
	}
	if len(teams[0].MemberIDs) != 3 || len(teams[1].MemberIDs) != 2 {
		t.Fatalf("members = %+v", teams)
	}
	// A user removed from the directory leaves their teams, as a disabled AD account does.
	f.Remove("staff2")
	teams, _ = f.Teams(context.Background())
	if len(teams[1].MemberIDs) != 1 {
		t.Fatalf("after remove: %+v", teams[1])
	}
}

// With no base DN the LDAP source says teams are off rather than listing none,
// so the sync does not wipe every team.
func TestLDAPTeamsOffWithoutBaseDN(t *testing.T) {
	l, err := NewLDAP(LDAPConfig{URL: "ldaps://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Teams(context.Background()); !errors.Is(err, ErrTeamsOff) {
		t.Fatalf("Teams() = %v, want ErrTeamsOff", err)
	}
}

// TestLiveLDAP runs against a real DC. Opt in with LDAP_LIVE=1 plus the usual
// LDAP_* variables and LDAP_LIVE_USER / LDAP_LIVE_PASSWORD, e.g.
//
//	set -a; source .env; set +a; LDAP_LIVE=1 LDAP_LIVE_USER=alex \
//	  LDAP_LIVE_PASSWORD="$(read -rs p; echo $p)" go test -run Live ./internal/directory/
func TestLiveLDAP(t *testing.T) {
	if os.Getenv("LDAP_LIVE") != "1" {
		t.Skip("set LDAP_LIVE=1 to run against a real directory")
	}
	d, err := NewLDAP(LDAPConfig{
		URL: os.Getenv("LDAP_URL"), BindDN: os.Getenv("LDAP_BIND_DN"),
		BindPassword: os.Getenv("LDAP_BIND_PASSWORD"), BaseDN: os.Getenv("LDAP_BASE_DN"),
		UserFilter: "(&(objectCategory=person)(objectClass=user)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))",
		AdminGroup: os.Getenv("LDAP_ADMIN_GROUP"), TeamsBaseDN: os.Getenv("LDAP_TEAMS_BASE_DN"), CAFile: os.Getenv("LDAP_CA_FILE"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, err := d.Authenticate(ctx, os.Getenv("LDAP_LIVE_USER"), os.Getenv("LDAP_LIVE_PASSWORD"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	t.Logf("signed in: %+v", u)
	if _, err := d.Authenticate(ctx, os.Getenv("LDAP_LIVE_USER"), "definitely-wrong-"+u.DN); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password gave %v", err)
	}
	users, err := d.AllUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	withManager := 0
	for _, u := range users {
		if u.ManagerDN != "" {
			withManager++
		}
	}
	t.Logf("%d users, %d with a manager", len(users), withManager)

	// Teams are searched only when LDAP_TEAMS_BASE_DN is set.
	if os.Getenv("LDAP_TEAMS_BASE_DN") == "" {
		t.Log("LDAP_TEAMS_BASE_DN unset: skipping the team search")
		return
	}
	teams, err := d.Teams(ctx)
	if err != nil {
		t.Fatalf("teams: %v", err)
	}
	known := map[string]bool{}
	for _, u := range users {
		known[u.ID] = true
	}
	for _, tm := range teams {
		if tm.ID == "" || tm.Name == "" {
			t.Errorf("team without an id or name: %+v", tm)
		}
		strangers := 0
		for _, m := range tm.MemberIDs {
			if !known[m] {
				strangers++
			}
		}
		t.Logf("team %q: %d members, %d not in the user listing", tm.Name, len(tm.MemberIDs), strangers)
	}
	t.Logf("%d teams", len(teams))
}

// AD tools show objectGUID with the first three groups byte-swapped from the
// wire order; matching them lets an admin cross-check an id against ADUC.
func TestGUIDString(t *testing.T) {
	b := []byte{0x78, 0x56, 0x34, 0x12, 0x34, 0x12, 0x78, 0x56, 0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56, 0x78}
	if got := guidString(b); got != "12345678-1234-5678-9abc-def012345678" {
		t.Fatalf("guidString = %s", got)
	}
	if guidString([]byte{1, 2}) != "" {
		t.Fatal("short GUID accepted")
	}
}
