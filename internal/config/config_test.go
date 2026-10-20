package config

import (
	"strings"
	"testing"
)

func setenv(t *testing.T, kv map[string]string) {
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// The fake directory accepts any password. It must not start on a deployment
// served over https, however the .env got there.
func TestFakeDirectoryRefusedWithSecureCookies(t *testing.T) {
	setenv(t, map[string]string{"DIRECTORY": "fake", "COOKIE_SECURE": "true"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DIRECTORY=fake") {
		t.Fatalf("fake + secure cookies: err = %v", err)
	}
	t.Setenv("COOKIE_SECURE", "false")
	if _, err := Load(); err != nil {
		t.Fatalf("fake for local dev: %v", err)
	}
}

// The team feature is off unless its base DN is given, and the DN is trimmed.
func TestTeamsBaseDN(t *testing.T) {
	setenv(t, map[string]string{"DIRECTORY": "fake", "COOKIE_SECURE": "false"})
	c, err := Load()
	if err != nil || c.Directory.TeamsBaseDN != "" {
		t.Fatalf("unset: %q, %v", c.Directory.TeamsBaseDN, err)
	}
	t.Setenv("LDAP_TEAMS_BASE_DN", " OU=Teams,DC=x ")
	if c, err = Load(); err != nil || c.Directory.TeamsBaseDN != "OU=Teams,DC=x" {
		t.Fatalf("set: %q, %v", c.Directory.TeamsBaseDN, err)
	}
}

func TestPartialConfigRefused(t *testing.T) {
	base := map[string]string{"DIRECTORY": "fake", "COOKIE_SECURE": "false"}
	cases := map[string]map[string]string{
		"graph without secret":   {"GRAPH_TENANT_ID": "t", "GRAPH_CLIENT_ID": "c"},
		"graph without mailbox":  {"GRAPH_TENANT_ID": "t", "GRAPH_CLIENT_ID": "c", "GRAPH_CLIENT_SECRET": "s", "EMAIL_FROM": "a@b"},
		"ldap missing bind":      {"DIRECTORY": "ldap", "LDAP_URL": "ldaps://dc", "LDAP_BASE_DN": "dc=x"},
		"unknown directory kind": {"DIRECTORY": "entra"},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			setenv(t, base)
			setenv(t, extra)
			if _, err := Load(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}
