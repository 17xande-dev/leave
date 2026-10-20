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
		AdminGroup: os.Getenv("LDAP_ADMIN_GROUP"), CAFile: os.Getenv("LDAP_CA_FILE"),
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
