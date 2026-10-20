// Package config reads every setting from the environment into one struct.
// Nothing else in the program calls os.Getenv, so this file is the complete
// list of knobs and .env.example mirrors it.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is deployment wiring and secrets only. Policy an admin would change
// (the working week, the final approver, sick-leave rules) lives in the
// database and is edited on Admin -> Settings.
type Config struct {
	ListenAddr string
	DBPath     string

	// BaseURL is the origin people reach the app at (https://leave.example.org).
	// It is used for links in emails, which have no request to derive one from,
	// and for the CSRF origin check.
	BaseURL string

	// CookieSecure is true unless the app is served over plain http, which is
	// only ever local development.
	CookieSecure bool
	SessionTTL   time.Duration

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
	BindPassword string
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
	ClientSecret string
	// From is the mailbox notifications are sent as.
	From string
	// CalendarMailbox owns the shared leave calendar; CalendarID picks a
	// calendar inside it other than the default one.
	CalendarMailbox string
	CalendarID      string
}

// Configured reports whether Graph can be used at all.
func (g Graph) Configured() bool {
	return g.TenantID != "" && g.ClientID != "" && g.ClientSecret != ""
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:     getenv("LISTEN_ADDR", ":8080"),
		DBPath:         getenv("DB_PATH", "data/leave.db"),
		BaseURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("BASE_URL")), "/"),
		CookieSecure:   getenv("COOKIE_SECURE", "true") == "true",
		TrustedProxies: parseList(os.Getenv("TRUSTED_PROXIES")),
		Directory: Directory{
			Kind:               getenv("DIRECTORY", "ldap"),
			FakeFile:           getenv("FAKE_DIRECTORY_FILE", "testdata/directory.json"),
			URL:                strings.TrimSpace(os.Getenv("LDAP_URL")),
			BindDN:             strings.TrimSpace(os.Getenv("LDAP_BIND_DN")),
			BindPassword:       os.Getenv("LDAP_BIND_PASSWORD"),
			BaseDN:             strings.TrimSpace(os.Getenv("LDAP_BASE_DN")),
			UserFilter:         getenv("LDAP_USER_FILTER", "(&(objectCategory=person)(objectClass=user)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"),
			AdminGroup:         strings.TrimSpace(os.Getenv("LDAP_ADMIN_GROUP")),
			TeamsBaseDN:        strings.TrimSpace(os.Getenv("LDAP_TEAMS_BASE_DN")),
			CAFile:             strings.TrimSpace(os.Getenv("LDAP_CA_FILE")),
			InsecureSkipVerify: os.Getenv("LDAP_INSECURE_SKIP_VERIFY") == "true",
		},
		Graph: Graph{
			TenantID:        strings.TrimSpace(os.Getenv("GRAPH_TENANT_ID")),
			ClientID:        strings.TrimSpace(os.Getenv("GRAPH_CLIENT_ID")),
			ClientSecret:    os.Getenv("GRAPH_CLIENT_SECRET"),
			From:            strings.TrimSpace(os.Getenv("EMAIL_FROM")),
			CalendarMailbox: strings.TrimSpace(os.Getenv("LEAVE_CALENDAR_MAILBOX")),
			CalendarID:      strings.TrimSpace(os.Getenv("LEAVE_CALENDAR_ID")),
		},
	}

	var err error
	if c.SessionTTL, err = duration("SESSION_TTL", 8*time.Hour); err != nil {
		return Config{}, err
	}
	if c.Directory.SyncInterval, err = duration("DIRECTORY_SYNC_INTERVAL", time.Hour); err != nil {
		return Config{}, err
	}
	if c.Location, err = time.LoadLocation(getenv("TZ_NAME", "Africa/Johannesburg")); err != nil {
		return Config{}, fmt.Errorf("config: TZ_NAME: %w", err)
	}

	switch c.Directory.Kind {
	case "fake":
		// A fake directory accepts any password. Refusing it whenever cookies
		// are marked Secure -- i.e. whenever this is served over https, which
		// is every real deployment -- keeps a copied dev .env from quietly
		// opening the door.
		if c.CookieSecure {
			return Config{}, fmt.Errorf("config: DIRECTORY=fake accepts any password and is " +
				"only allowed with COOKIE_SECURE=false (local development)")
		}
	case "ldap":
		d := c.Directory
		if d.URL == "" || d.BindDN == "" || d.BindPassword == "" || d.BaseDN == "" {
			return Config{}, fmt.Errorf("config: LDAP_URL, LDAP_BIND_DN, LDAP_BIND_PASSWORD " +
				"and LDAP_BASE_DN are required when DIRECTORY=ldap")
		}
	default:
		return Config{}, fmt.Errorf("config: DIRECTORY must be ldap or fake, got %q", c.Directory.Kind)
	}

	// Graph is all or nothing: a half-configured registration is caught at
	// boot rather than at the first approval nobody hears about.
	g := c.Graph
	if (g.TenantID != "" || g.ClientID != "" || g.ClientSecret != "") && !g.Configured() {
		return Config{}, fmt.Errorf("config: GRAPH_TENANT_ID, GRAPH_CLIENT_ID and " +
			"GRAPH_CLIENT_SECRET must all be set, or none of them")
	}
	if g.Configured() && (g.From == "" || g.CalendarMailbox == "") {
		return Config{}, fmt.Errorf("config: EMAIL_FROM and LEAVE_CALENDAR_MAILBOX are " +
			"required when Graph is configured")
	}

	c.DevSMTP.Host = strings.TrimSpace(os.Getenv("DEV_SMTP_HOST"))
	c.DevSMTP.From = getenv("EMAIL_FROM", "leave@localhost")
	if c.DevSMTP.Port, err = integer("DEV_SMTP_PORT", 1025); err != nil {
		return Config{}, err
	}

	return c, nil
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive duration like 8h, got %q", key, v)
	}
	return d, nil
}

func integer(key string, fallback int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("config: %s must be a non-negative integer, got %q", key, v)
	}
	return n, nil
}

func parseList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
