package directory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAPConfig describes the DC and the read-only service account used to find
// people. The service account never verifies anyone's password: that is a
// bind as the user themselves.
type LDAPConfig struct {
	URL          string
	BindDN       string
	BindPassword string
	BaseDN       string
	UserFilter   string
	AdminGroup   string
	// TeamsBaseDN is where the volunteer-team groups live. Empty turns teams
	// off.
	TeamsBaseDN string
	CAFile      string
	// InsecureSkipVerify is for a lab DC with a self-signed certificate.
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// LDAP is a Directory backed by Active Directory.
//
// It dials per operation rather than holding a pool: sign-ins are a handful an
// hour, and a fresh connection cannot be left in a half-bound state by a
// previous caller -- the classic pooled-LDAP bug where one user's bind leaks
// into the next request.
type LDAP struct {
	cfg LDAPConfig
	tls *tls.Config
}

// The attributes read for every person.
var userAttrs = []string{"objectGUID", "sAMAccountName", "displayName", "cn", "mail", "department", "manager"}

// matchingRuleInChain makes a memberOf test follow nested groups, so putting
// a group inside the admin group works the way an AD admin expects it to.
const matchingRuleInChain = "1.2.840.113556.1.4.1941"

func NewLDAP(cfg LDAPConfig) (*LDAP, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Host == "" {
		return nil, fmt.Errorf("ldap: URL must be ldaps://host[:636] or ldap://host[:389], got %q", cfg.URL)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	host := u.Hostname()
	tc := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ldap: read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ldap: no certificates found in %s", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	return &LDAP{cfg: cfg, tls: tc}, nil
}

// dial connects and secures the connection. Plain ldap:// is upgraded with
// StartTLS and refused if the upgrade fails: passwords are never sent to the
// DC in the clear.
func (d *LDAP) dial() (*ldap.Conn, error) {
	conn, err := ldap.DialURL(d.cfg.URL,
		ldap.DialWithDialer(&net.Dialer{Timeout: d.cfg.Timeout}),
		ldap.DialWithTLSConfig(d.tls))
	if err != nil {
		return nil, fmt.Errorf("ldap: dial: %w", err)
	}
	conn.SetTimeout(d.cfg.Timeout)
	if strings.HasPrefix(d.cfg.URL, "ldap://") {
		if err := conn.StartTLS(d.tls); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ldap: StartTLS: %w", err)
		}
	}
	return conn, nil
}

// serviceConn is a connection bound as the service account.
func (d *LDAP) serviceConn() (*ldap.Conn, error) {
	conn, err := d.dial()
	if err != nil {
		return nil, err
	}
	if err := conn.Bind(d.cfg.BindDN, d.cfg.BindPassword); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ldap: service account bind: %w", err)
	}
	return conn, nil
}

func (d *LDAP) Authenticate(_ context.Context, username, password string) (User, error) {
	// AD treats a simple bind with a DN and an empty password as an anonymous
	// bind, and reports success. go-ldap refuses it too, but this is the one
	// check whose absence would let anybody in as anybody, so it is not left
	// to a library default.
	username = NormalizeUsername(username)
	if password == "" || username == "" {
		return User{}, ErrInvalidCredentials
	}

	conn, err := d.serviceConn()
	if err != nil {
		return User{}, err
	}
	defer conn.Close()

	filter := fmt.Sprintf("(&%s(sAMAccountName=%s))", d.cfg.UserFilter, ldap.EscapeFilter(username))
	res, err := conn.Search(ldap.NewSearchRequest(d.cfg.BaseDN, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 2, 0, false, filter, userAttrs, nil))
	if err != nil {
		return User{}, fmt.Errorf("ldap: find user: %w", err)
	}
	if len(res.Entries) != 1 {
		// Unknown, disabled (the filter excludes those), or ambiguous.
		return User{}, ErrInvalidCredentials
	}
	entry := res.Entries[0]

	// Verify the password on the same connection by rebinding as the user.
	// It is closed straight afterwards, so the user's identity is never used
	// for anything else.
	if err := conn.Bind(entry.DN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("ldap: user bind: %w", err)
	}

	u := userFromEntry(entry)
	if u.ID == "" {
		return User{}, fmt.Errorf("ldap: %s has no objectGUID", entry.DN)
	}
	if d.cfg.AdminGroup != "" {
		// Checked with a fresh service bind: the user's own account may not
		// be allowed to read group membership.
		admins, err := d.adminDNs()
		if err != nil {
			return User{}, err
		}
		u.Admin = admins[strings.ToLower(u.DN)]
	}
	return u, nil
}

func (d *LDAP) AllUsers(context.Context) ([]User, error) {
	conn, err := d.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Paged, because AD caps an unpaged search at 1000 results and returns
	// the first thousand as if that were everyone.
	res, err := conn.SearchWithPaging(ldap.NewSearchRequest(d.cfg.BaseDN, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 0, 0, false, d.cfg.UserFilter, userAttrs, nil), 500)
	if err != nil {
		return nil, fmt.Errorf("ldap: list users: %w", err)
	}
	var admins map[string]bool
	if d.cfg.AdminGroup != "" {
		if admins, err = d.adminDNsOn(conn); err != nil {
			return nil, err
		}
	}
	out := make([]User, 0, len(res.Entries))
	for _, e := range res.Entries {
		u := userFromEntry(e)
		if u.Username == "" || u.ID == "" {
			continue
		}
		u.Admin = admins[strings.ToLower(u.DN)]
		out = append(out, u)
	}
	return out, nil
}

func (d *LDAP) adminDNs() (map[string]bool, error) {
	conn, err := d.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return d.adminDNsOn(conn)
}

// adminDNsOn returns the lower-cased DNs of every (transitive) member of the
// admin group.
func (d *LDAP) adminDNsOn(conn *ldap.Conn) (map[string]bool, error) {
	filter := fmt.Sprintf("(&%s(memberOf:%s:=%s))", d.cfg.UserFilter, matchingRuleInChain,
		ldap.EscapeFilter(d.cfg.AdminGroup))
	res, err := conn.SearchWithPaging(ldap.NewSearchRequest(d.cfg.BaseDN, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 0, 0, false, filter, []string{"dn"}, nil), 500)
	if err != nil {
		return nil, fmt.Errorf("ldap: admin group members: %w", err)
	}
	out := make(map[string]bool, len(res.Entries))
	for _, e := range res.Entries {
		out[strings.ToLower(e.DN)] = true
	}
	return out, nil
}

// Teams lists the AD groups under TeamsBaseDN with their enabled members.
// Each group is searched for its members with the chain rule, so a group
// nested in a team counts, as it does for the admin group.
func (d *LDAP) Teams(context.Context) ([]Team, error) {
	if d.cfg.TeamsBaseDN == "" {
		return nil, ErrTeamsOff
	}
	conn, err := d.serviceConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// Paged for the same reason as the user list.
	groups, err := conn.SearchWithPaging(ldap.NewSearchRequest(d.cfg.TeamsBaseDN, ldap.ScopeWholeSubtree,
		ldap.NeverDerefAliases, 0, 0, false, "(objectClass=group)",
		[]string{"objectGUID", "cn", "description"}, nil), 500)
	if err != nil {
		return nil, fmt.Errorf("ldap: list teams: %w", err)
	}
	out := make([]Team, 0, len(groups.Entries))
	for _, g := range groups.Entries {
		id := guidString(g.GetRawAttributeValue("objectGUID"))
		if id == "" {
			continue
		}
		name := strings.TrimSpace(g.GetAttributeValue("description"))
		if name == "" {
			name = g.GetAttributeValue("cn")
		}
		filter := fmt.Sprintf("(&%s(memberOf:%s:=%s))", d.cfg.UserFilter, matchingRuleInChain, ldap.EscapeFilter(g.DN))
		members, err := conn.SearchWithPaging(ldap.NewSearchRequest(d.cfg.BaseDN, ldap.ScopeWholeSubtree,
			ldap.NeverDerefAliases, 0, 0, false, filter, []string{"objectGUID"}, nil), 500)
		if err != nil {
			return nil, fmt.Errorf("ldap: members of %s: %w", g.DN, err)
		}
		t := Team{ID: id, Name: name}
		for _, m := range members.Entries {
			if mid := guidString(m.GetRawAttributeValue("objectGUID")); mid != "" {
				t.MemberIDs = append(t.MemberIDs, mid)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func userFromEntry(e *ldap.Entry) User {
	name := e.GetAttributeValue("displayName")
	if name == "" {
		name = e.GetAttributeValue("cn")
	}
	return User{
		ID:         guidString(e.GetRawAttributeValue("objectGUID")),
		DN:         e.DN,
		Username:   NormalizeUsername(e.GetAttributeValue("sAMAccountName")),
		Name:       name,
		Email:      strings.TrimSpace(e.GetAttributeValue("mail")),
		Department: strings.TrimSpace(e.GetAttributeValue("department")),
		ManagerDN:  e.GetAttributeValue("manager"),
	}
}

// guidString formats AD's 16-byte objectGUID the way AD tools display it: the
// first three groups are little-endian on the wire.
func guidString(b []byte) string {
	if len(b) != 16 {
		return ""
	}
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// Compile-time checks that both implementations satisfy the interface.
var (
	_ Directory = (*LDAP)(nil)
	_ Directory = (*Fake)(nil)

	_ TeamSource = (*LDAP)(nil)
	_ TeamSource = (*Fake)(nil)
)
