// Package directory is where people come from: who they are, which department
// they are in, who their manager is, and whether they administer the app.
//
// It is an interface because the source is expected to change. LDAP against
// the on-prem DC is the first implementation; Entra ID (OIDC sign-in, Graph
// /users for the sync) is the planned second, and it fills the same User.
package directory

import (
	"context"
	"errors"
	"strings"
)

// User is one person as the directory describes them.
type User struct {
	// ID is the directory's permanent id for the person: AD's objectGUID.
	// It survives renames and OU moves, which the DN and username do not.
	// (Entra ID exposes the same value as onPremisesImmutableId, which is what
	// makes a later move to Entra keep everyone's history.)
	ID string
	// DN is where the person sits now, and what a report's ManagerDN points at.
	DN         string
	Username   string // sAMAccountName, lower-cased
	Name       string
	Email      string
	Department string
	ManagerDN  string
	Admin      bool
}

// ErrInvalidCredentials is the one error a failed sign-in reports, whatever the
// cause -- unknown user, wrong password, disabled account. Which it was is not
// the visitor's business, and telling them turns the form into a way to find
// out which usernames exist.
var ErrInvalidCredentials = errors.New("directory: invalid username or password")

type Directory interface {
	// Authenticate verifies a password and returns the person it belongs to.
	Authenticate(ctx context.Context, username, password string) (User, error)
	// AllUsers lists every enabled person, for the periodic sync that lets
	// approvers be found (and emailed) before they have ever signed in.
	AllUsers(ctx context.Context) ([]User, error)
}

// Team is a group of volunteers as its source describes it.
type Team struct {
	// ID is the source's permanent id for the team (an AD group's objectGUID,
	// later a Planning Center team id): names repeat and change, ids do not.
	ID   string
	Name string
	// MemberIDs are directory user IDs (User.ID). People who are not users
	// here are simply never matched when the teams are stored.
	MemberIDs []string
}

// TeamSource is where volunteer teams come from. It is a separate interface
// from Directory because the source is expected to change on its own:
// Planning Center is the likely replacement for AD groups.
type TeamSource interface {
	// Teams lists every team. An empty list with a nil error means the source
	// has no teams, and is applied as that; a failure must be an error.
	Teams(ctx context.Context) ([]Team, error)
}

// ErrTeamsOff is what a source returns when the feature is not configured,
// so the sync skips teams instead of treating the lack of them as a result.
var ErrTeamsOff = errors.New("directory: teams are not configured")

// NormalizeUsername turns the forms people actually type -- "EXAMPLE\alex",
// "alex@example.org", " Alex " -- into the bare account name.
func NormalizeUsername(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}
