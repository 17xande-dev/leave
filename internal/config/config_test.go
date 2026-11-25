package config

import (
	"strconv"
	"strings"
	"testing"
)

func setenv(t *testing.T, kv map[string]string) {
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// ldap is a complete production-shaped environment: no fake directory, so no
// other check can mask the one a test is aimed at.
func ldap() map[string]string {
	return map[string]string{
		"DIRECTORY":          "ldap",
		"BASE_URL":           "https://leave.example.org",
		"LDAP_URL":           "ldaps://dc.example.test",
		"LDAP_BIND_DN":       "cn=bind",
		"LDAP_BIND_PASSWORD": "fixture",
		"LDAP_BASE_DN":       "dc=example,dc=test",
	}
}

// local is the smallest environment Load accepts: the fake directory over
// plain http, which is local development.
func local() map[string]string {
	return map[string]string{
		"DIRECTORY":     "fake",
		"COOKIE_SECURE": "false",
		"BASE_URL":      "http://localhost:8082",
	}
}

// The fake directory accepts any password. It must not start on a deployment
// served over https, however the .env got there.
func TestFakeDirectoryRefusedWithSecureCookies(t *testing.T) {
	setenv(t, map[string]string{"DIRECTORY": "fake", "COOKIE_SECURE": "true", "BASE_URL": "https://leave.example.org"})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DIRECTORY=fake") {
		t.Fatalf("fake + secure cookies: err = %v", err)
	}
	setenv(t, local())
	if _, err := Load(); err != nil {
		t.Fatalf("fake for local dev: %v", err)
	}
}

// The team feature is off unless its base DN is given, and the DN is trimmed.
func TestTeamsBaseDN(t *testing.T) {
	setenv(t, local())
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
	base := local()
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

func TestTrustedProxiesInvalidCIDRRefused(t *testing.T) {
	setenv(t, local())
	t.Setenv("TRUSTED_PROXIES", "172.30.17.0/24, nonsense")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXIES") {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("TRUSTED_PROXIES", "172.30.17.0/24")
	if c, err := Load(); err != nil || len(c.TrustedProxies) != 1 {
		t.Fatalf("valid value: %+v, %v", c.TrustedProxies, err)
	}
}

func TestBackupSettings(t *testing.T) {
	setenv(t, local())
	t.Setenv("DB_PATH", "/srv/leave/leave.db")
	t.Setenv("BACKUP_DIR", "")
	t.Setenv("BACKUP_KEEP", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupDir != "/srv/leave/backups" || c.BackupKeep != 14 {
		t.Fatalf("defaults: dir = %q keep = %d", c.BackupDir, c.BackupKeep)
	}

	t.Setenv("BACKUP_DIR", "/mnt/b")
	t.Setenv("BACKUP_KEEP", "0")
	if c, err = Load(); err != nil || c.BackupDir != "/mnt/b" || c.BackupKeep != 0 {
		t.Fatalf("explicit: %+v, err = %v", c, err)
	}

	t.Setenv("BACKUP_KEEP", "many")
	if _, err = Load(); err == nil {
		t.Fatal("BACKUP_KEEP=many accepted")
	}
}

// SESSION_TTL was a setting until session lifetime moved into websec
// (2 h idle, 12 h absolute). An old .env or deploy file may still carry it,
// so it must be ignored whatever it holds: failing startup over a setting that
// does nothing would turn a harmless leftover into an outage.
func TestRetiredSessionTTLIgnored(t *testing.T) {
	setenv(t, local())
	for _, v := range []string{"8h", "not-a-duration", "-5m", "0"} {
		t.Setenv("SESSION_TTL", v)
		if _, err := Load(); err != nil {
			t.Fatalf("SESSION_TTL=%q must be ignored, Load failed: %v", v, err)
		}
	}
}

// A boolean that is not exactly true or false stops start-up. The old reading
// treated everything but "true" as false, so a typo in COOKIE_SECURE served
// non-Secure cookies and, worse, switched off the DIRECTORY=fake guard that
// only fires while cookies are Secure. Empty still means the default (true).
func TestBooleansAreStrict(t *testing.T) {
	bad := []string{"ture", "True", "TRUE", "1", "0", "yes", "on", " true", "true ", "\ttrue"}
	for _, key := range []string{"COOKIE_SECURE", "LDAP_INSECURE_SKIP_VERIFY"} {
		for _, v := range bad {
			t.Run(key+"="+strconv.Quote(v), func(t *testing.T) {
				setenv(t, ldap())
				t.Setenv(key, v)
				_, err := Load()
				if err == nil {
					t.Fatalf("%s=%q accepted", key, v)
				}
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("error does not name the key: %v", err)
				}
			})
		}
	}
}

// The value is never repeated: a typo may be a secret pasted on the wrong line.
func TestBooleanErrorDoesNotEchoValue(t *testing.T) {
	for _, key := range []string{"COOKIE_SECURE", "LDAP_INSECURE_SKIP_VERIFY"} {
		t.Run(key, func(t *testing.T) {
			setenv(t, ldap())
			t.Setenv(key, "hunter2-fixture")
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The reason strictness matters: a fake directory behind a mistyped
// COOKIE_SECURE must be refused, not waved through.
func TestFakeDirectoryGuardSurvivesTypo(t *testing.T) {
	for _, v := range []string{"ture", "True", "1", "false "} {
		setenv(t, local())
		t.Setenv("BASE_URL", "https://leave.example.org")
		t.Setenv("COOKIE_SECURE", v)
		if _, err := Load(); err == nil {
			t.Fatalf("DIRECTORY=fake with COOKIE_SECURE=%q started", v)
		}
	}
}

func TestBooleansExactSpellingsAndDefaults(t *testing.T) {
	setenv(t, ldap())
	t.Setenv("COOKIE_SECURE", "")
	c, err := Load()
	if err != nil || !c.CookieSecure || c.Directory.InsecureSkipVerify {
		t.Fatalf("empty means default (secure, verify): %+v, %v", c.CookieSecure, err)
	}
	t.Setenv("LDAP_INSECURE_SKIP_VERIFY", "true")
	if c, err = Load(); err != nil || !c.Directory.InsecureSkipVerify {
		t.Fatalf("true: %v", err)
	}
	t.Setenv("LDAP_INSECURE_SKIP_VERIFY", "false")
	if c, err = Load(); err != nil || c.Directory.InsecureSkipVerify {
		t.Fatalf("false: %v", err)
	}
	t.Setenv("COOKIE_SECURE", "true")
	if c, err = Load(); err != nil || !c.CookieSecure {
		t.Fatalf("true: %v", err)
	}
}

// BASE_URL is checked with the same rules the CSRF layer applies, so a value
// csrf.New would refuse stops Load instead of surfacing after the database is
// opened and migrated.
func TestBaseURLValidatedInLoad(t *testing.T) {
	cases := []struct {
		name, secure, url string
		ok                bool
	}{
		{"empty", "false", "", false},
		{"http under secure cookies", "true", "http://leave.example.org", false},
		{"leading zero port", "true", "https://leave.example.org:0443", false},
		{"default port", "true", "https://leave.example.org:443", false},
		{"uppercase", "true", "https://Leave.Example.org", false},
		{"path", "true", "https://leave.example.org/app", false},
		{"userinfo", "true", "https://user:pw@leave.example.org", false},
		{"no scheme", "true", "leave.example.org", false},
		{"https secure", "true", "https://leave.example.org", true},
		{"trailing slash is trimmed as before", "true", "https://leave.example.org/", true},
		{"http local", "false", "http://localhost:8082", true},
		{"https with insecure cookies", "false", "https://leave.example.org", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setenv(t, ldap())
			t.Setenv("COOKIE_SECURE", tc.secure)
			t.Setenv("BASE_URL", tc.url)
			c, err := Load()
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if strings.HasSuffix(c.BaseURL, "/") {
					t.Fatalf("BaseURL = %q", c.BaseURL)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "BASE_URL") {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "pw") {
				t.Fatalf("error echoes userinfo: %v", err)
			}
		})
	}
}
