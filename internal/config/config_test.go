package config

import (
	"strings"
	"testing"
	"time"
)

func setenv(t *testing.T, kv map[string]string) {
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// The fake directory accepts any password. It must not start on a deployment
// served over https, however the .env got there.
func TestFakeDirectoryRefusedWithSecureCookies(t *testing.T) {
	setenv(t, map[string]string{"DIRECTORY": "fake", "FINAL_APPROVER": "p", "COOKIE_SECURE": "true"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DIRECTORY=fake") {
		t.Fatalf("fake + secure cookies: err = %v", err)
	}
	t.Setenv("COOKIE_SECURE", "false")
	if _, err := Load(); err != nil {
		t.Fatalf("fake for local dev: %v", err)
	}
}

func TestPartialConfigRefused(t *testing.T) {
	base := map[string]string{"DIRECTORY": "fake", "FINAL_APPROVER": "p", "COOKIE_SECURE": "false"}
	cases := map[string]map[string]string{
		"graph without secret":   {"GRAPH_TENANT_ID": "t", "GRAPH_CLIENT_ID": "c"},
		"graph without mailbox":  {"GRAPH_TENANT_ID": "t", "GRAPH_CLIENT_ID": "c", "GRAPH_CLIENT_SECRET": "s", "EMAIL_FROM": "a@b"},
		"ldap missing bind":      {"DIRECTORY": "ldap", "LDAP_URL": "ldaps://dc", "LDAP_BASE_DN": "dc=x"},
		"no final approver":      {"FINAL_APPROVER": ""},
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

func TestWorkWeekConfig(t *testing.T) {
	setenv(t, map[string]string{"DIRECTORY": "fake", "FINAL_APPROVER": "p", "COOKIE_SECURE": "false",
		"WORK_DAYS": "tue,wed,thu,fri,sun", "RESTRICTED_DAY": "sunday"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.WorkDays) != 5 || c.RestrictedDay == nil || *c.RestrictedDay != time.Sunday || c.RestrictedMinDays != 3 {
		t.Fatalf("work week config = %v %v %d", c.WorkDays, c.RestrictedDay, c.RestrictedMinDays)
	}
	// A restricted day nobody works would be a rule that never applies:
	// almost certainly a typo, so refuse it.
	t.Setenv("RESTRICTED_DAY", "mon")
	if _, err := Load(); err == nil {
		t.Fatal("restricted day outside the working week accepted")
	}
	t.Setenv("RESTRICTED_DAY", "")
	t.Setenv("WORK_DAYS", "tue,funday")
	if _, err := Load(); err == nil {
		t.Fatal("unknown day accepted")
	}
}
