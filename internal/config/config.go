// Package config reads every setting from the environment into one struct.
// Nothing else in the program calls os.Getenv, so this file is the complete
// list of knobs and .env.example mirrors it.
package config

import (
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/17xande-dev/env"
	"github.com/17xande-dev/websec/csrf"
)

// Config is deployment wiring and secrets only. Policy an admin would change
// (the working week, the final approver, sick-leave rules) lives in the
// database and is edited on Admin -> Settings.
type Config struct {
	ListenAddr string
	DBPath     string

	// BackupDir holds the daily database backups (BACKUP_DIR; default
	// "backups" beside the database file). BackupKeep is how many to retain
	// (BACKUP_KEEP; default 14); 0 turns backups off.
	BackupDir  string
	BackupKeep int

	// BaseURL is the origin people reach the app at (https://leave.example.org).
	// It is used for links in emails, which have no request to derive one from,
	// and for the CSRF origin check.
	BaseURL string

	// CookieSecure is true unless the app is served over plain http, which is
	// only ever local development.
	CookieSecure bool

	// TrustedProxies are the CIDRs allowed to set X-Forwarded-For, so the login
	// rate limit keys on the real client rather than the proxy.
	TrustedProxies []string

	// Location is the timezone "today" is decided in. Leave is a matter of
	// calendar dates in South Africa; a UTC server deciding that tomorrow has
	// already started at 02:00 local time would refuse valid requests.
	Location *time.Location

	Directory Directory
	Graph     Graph
	// DevSMTP sends mail to a local sink (mailpit) when Graph is not
	// configured. It is a development convenience, not a production transport:
	// no authentication, and TLS off by default.
	DevSMTP DevSMTP
}

// Directory selects and configures where users come from.
type Directory struct {
	// Kind is "ldap" or "fake". Fake reads users from FakeFile and accepts any
	// password; it exists for development and tests and is refused when
	// CookieSecure is on, so it cannot reach a real deployment by accident.
	Kind     string
	FakeFile string

	URL          string // ldaps://dc1.example.local:636 or ldap://…:389 (StartTLS is then required)
	BindDN       string
	BindPassword env.Secret
	BaseDN       string
	// UserFilter restricts which accounts are people who take leave. The
	// default excludes disabled accounts and non-person objects.
	UserFilter string
	// AdminGroup is the DN of the AD group whose members administer the app.
	AdminGroup string
	// TeamsBaseDN is the OU holding the AD groups that are volunteer teams.
	// Empty turns the team feature off.
	TeamsBaseDN string
	// CAFile is a PEM bundle for an internal CA; empty uses the system roots.
	CAFile string
	// InsecureSkipVerify is for a lab DC with a self-signed certificate only.
	InsecureSkipVerify bool

	SyncInterval time.Duration
}

// Graph is the Entra ID app registration used for mail and the shared calendar.
type DevSMTP struct {
	Host string
	Port int
	From string
}

type Graph struct {
	TenantID     string
	ClientID     string
	ClientSecret env.Secret
	// From is the mailbox notifications are sent as.
	From string
	// CalendarMailbox owns the shared leave calendar; CalendarID picks a
	// calendar inside it other than the default one.
	CalendarMailbox string
	CalendarID      string
}

// Configured reports whether Graph can be used at all.
func (g Graph) Configured() bool {
	return g.TenantID != "" && g.ClientID != "" && g.ClientSecret.IsSet()
}

const defaultDBPath = "data/leave.db"

// DatabasePath reads the shared database location without requiring server
// credentials, so migration status also works in an operator's local shell.
func DatabasePath() string {
	if path := strings.TrimSpace(os.Getenv("DB_PATH")); path != "" {
		return path
	}
	return defaultDBPath
}

// Load reads the environment. SESSION_TTL is retired: session lifetime comes
// from websec's session.Default, and a leftover SESSION_TTL is ignored.
func Load() (Config, error) { return load(os.LookupEnv) }

func load(lookup env.Lookup) (Config, error) {
	r := env.New(func(key string) (string, bool) {
		value, ok := lookup(key)
		// These settings historically trimmed operator whitespace before
		// applying their defaults. Booleans and credentials keep exact input.
		switch key {
		case "LISTEN_ADDR", "DB_PATH", "BACKUP_DIR", "BACKUP_KEEP", "BASE_URL",
			"DIRECTORY", "FAKE_DIRECTORY_FILE", "LDAP_URL", "LDAP_BIND_DN", "LDAP_BASE_DN",
			"LDAP_USER_FILTER", "LDAP_ADMIN_GROUP", "LDAP_TEAMS_BASE_DN", "LDAP_CA_FILE",
			"DIRECTORY_SYNC_INTERVAL", "GRAPH_TENANT_ID", "GRAPH_CLIENT_ID", "EMAIL_FROM",
			"LEAVE_CALENDAR_MAILBOX", "LEAVE_CALENDAR_ID", "DEV_SMTP_HOST", "DEV_SMTP_PORT", "TZ_NAME":
			value = strings.TrimSpace(value)
		}
		return value, ok
	})
	from := r.String("EMAIL_FROM", "")
	c := Config{
		ListenAddr:     r.String("LISTEN_ADDR", ":8080"),
		DBPath:         r.String("DB_PATH", defaultDBPath),
		BaseURL:        strings.TrimRight(r.String("BASE_URL", ""), "/"),
		CookieSecure:   r.Bool("COOKIE_SECURE", true),
		TrustedProxies: r.List("TRUSTED_PROXIES", nil),
		BackupKeep:     r.Int("BACKUP_KEEP", 14, 0, math.MaxInt),
		Directory: Directory{
			Kind:               r.String("DIRECTORY", "ldap"),
			FakeFile:           r.String("FAKE_DIRECTORY_FILE", "testdata/directory.json"),
			URL:                r.String("LDAP_URL", ""),
			BindDN:             r.String("LDAP_BIND_DN", ""),
			BindPassword:       r.Secret("LDAP_BIND_PASSWORD"),
			BaseDN:             r.String("LDAP_BASE_DN", ""),
			UserFilter:         r.String("LDAP_USER_FILTER", "(&(objectCategory=person)(objectClass=user)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"),
			AdminGroup:         r.String("LDAP_ADMIN_GROUP", ""),
			TeamsBaseDN:        r.String("LDAP_TEAMS_BASE_DN", ""),
			CAFile:             r.String("LDAP_CA_FILE", ""),
			InsecureSkipVerify: r.Bool("LDAP_INSECURE_SKIP_VERIFY", false),
			SyncInterval:       r.Duration("DIRECTORY_SYNC_INTERVAL", time.Hour, time.Nanosecond, 0),
		},
		Graph: Graph{
			TenantID:        r.String("GRAPH_TENANT_ID", ""),
			ClientID:        r.String("GRAPH_CLIENT_ID", ""),
			ClientSecret:    r.Secret("GRAPH_CLIENT_SECRET"),
			From:            from,
			CalendarMailbox: r.String("LEAVE_CALENDAR_MAILBOX", ""),
			CalendarID:      r.String("LEAVE_CALENDAR_ID", ""),
		},
		DevSMTP: DevSMTP{
			Host: r.String("DEV_SMTP_HOST", ""),
			Port: r.Int("DEV_SMTP_PORT", 1025, 0, math.MaxInt),
			From: from,
		},
	}
	if c.DevSMTP.From == "" {
		c.DevSMTP.From = "leave@localhost"
	}
	c.BackupDir = r.String("BACKUP_DIR", filepath.Join(filepath.Dir(c.DBPath), "backups"))
	location, err := time.LoadLocation(r.String("TZ_NAME", "Africa/Johannesburg"))
	if err != nil {
		r.Fail("TZ_NAME", "must be a known time zone")
	}
	c.Location = location

	// A bad origin must stop startup before db.Open creates or migrates the database.
	if err := csrf.ValidateOrigin(c.BaseURL, !c.CookieSecure); err != nil {
		r.Fail("BASE_URL", "must be a canonical origin (HTTPS when COOKIE_SECURE is true)")
	}
	for _, cidr := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			r.Fail("TRUSTED_PROXIES", "must be a comma-separated list of valid CIDRs")
			break
		}
	}

	switch c.Directory.Kind {
	case "fake":
		// A fake directory accepts any password. Refusing it whenever cookies
		// are marked Secure -- i.e. whenever this is served over https, which
		// is every real deployment -- keeps a copied dev .env from quietly
		// opening the door.
		if c.CookieSecure {
			r.Fail("DIRECTORY", "DIRECTORY=fake accepts any password and requires COOKIE_SECURE=false (local development)")
		}
	case "ldap":
		d := c.Directory
		if d.URL == "" || d.BindDN == "" || !d.BindPassword.IsSet() || d.BaseDN == "" {
			r.Fail("DIRECTORY", "LDAP_URL, LDAP_BIND_DN, LDAP_BIND_PASSWORD and LDAP_BASE_DN are required for LDAP")
		}
	default:
		r.Fail("DIRECTORY", "must be ldap or fake")
	}

	// Graph is all or nothing: a half-configured registration is caught at
	// boot rather than at the first approval nobody hears about.
	g := c.Graph
	if (g.TenantID != "" || g.ClientID != "" || g.ClientSecret.IsSet()) && !g.Configured() {
		r.Fail("GRAPH_CLIENT_SECRET", "GRAPH_TENANT_ID, GRAPH_CLIENT_ID and GRAPH_CLIENT_SECRET must all be set, or none of them")
	}
	if g.Configured() && (g.From == "" || g.CalendarMailbox == "") {
		r.Fail("EMAIL_FROM", "EMAIL_FROM and LEAVE_CALENDAR_MAILBOX are required when Graph is configured")
	}

	if err := r.Err(); err != nil {
		return Config{}, err
	}
	return c, nil
}
