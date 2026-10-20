package leave

import (
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/directory"
)

// team makes (or replaces) a team of these fixture people and syncs the
// source's whole listing, as the hourly sync does.
func (f *fixture) team(name string, who ...string) {
	f.t.Helper()
	t := directory.Team{ID: "team-" + name, Name: name}
	for _, w := range who {
		t.MemberIDs = append(t.MemberIDs, f.people[w].DN) // the fixture's directory id is the DN
	}
	replaced := false
	for i := range f.teams {
		if f.teams[i].ID == t.ID {
			f.teams[i], replaced = t, true
		}
	}
	if !replaced {
		f.teams = append(f.teams, t)
	}
	if _, _, err := f.us.SyncTeams(f.ctx, "fake", f.teams); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) teamID(name string) int64 {
	f.t.Helper()
	var id int64
	if err := f.db.QueryRow(`SELECT id FROM teams WHERE name = ?`, name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) teamRule(name string, percent int) {
	f.t.Helper()
	if err := f.svc.SetTeamRule(f.ctx, f.teamID(name), percent); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) teamDefault(percent int) {
	f.t.Helper()
	if err := f.svc.SetTeamDefault(f.ctx, f.people["hr"], percent); err != nil {
		f.t.Fatal(err)
	}
}

// Everyone is in one department with no limit; the Band is limited on its own.
func TestTeamLimitBlocksWhileDepartmentPasses(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "hr", "head", "orphan")
	f.rule("Ops", 100)
	f.team("Band", "staff", "hr", "head")
	f.teamRule("Band", 50)

	// One of three, then two of three, is over 50%.
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	_, err := f.submit("hr", "annual", "2026-10-12", "2026-10-12")
	if !fieldErr(err, "coverage") {
		t.Fatalf("second bandmember: %v", err)
	}
	msg := err.(FieldErrors)["coverage"]
	for _, want := range []string{"67% of the Band team (2 of 3) would be away on Mon 12 Oct. The limit is 50%.", "special permission"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
	// A person outside the team is not touched by its limit.
	f.mustSubmit("orphan", "annual", "2026-10-12", "2026-10-12")
	qt, err := f.svc.Preview(f.ctx, f.people["hr"], Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-12"})
	canOverride, _ := Offers(qt, err.(FieldErrors))
	if len(qt.Coverage) != 1 || qt.Coverage[0].Kind != CoverageTeam || !canOverride {
		t.Fatalf("preview: %+v, can override %v", qt.Coverage, canOverride)
	}
	// Sick leave counts but is not blocked.
	f.mustSubmit("hr", "sick", "2026-10-12", "2026-10-12")
}

func TestTwoTeamsOverTheLimitGiveTwoWarnings(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr")
	f.team("Choir", "staff", "head")
	f.teamRule("Band", 50)
	f.teamRule("Choir", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12")
	f.mustSubmit("head", "annual", "2026-10-12", "2026-10-12")

	_, err := f.submit("staff", "annual", "2026-10-12", "2026-10-12")
	msg, _ := err.(FieldErrors)["coverage"]
	if !strings.Contains(msg, "the Band team (2 of 2)") || !strings.Contains(msg, "the Choir team (2 of 2)") ||
		strings.Count(msg, "You can ask for special permission.") != 1 {
		t.Fatalf("message = %q", msg)
	}
	// With the department over as well, that is a third, listed first.
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	qt, _ := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-12"})
	if len(qt.Coverage) != 3 || qt.Coverage[0].Kind != CoverageDepartment || qt.Coverage[1].Name != "Band" || qt.Coverage[2].Name != "Choir" {
		t.Fatalf("coverage = %+v", qt.Coverage)
	}
}

// Special permission over a team limit climbs the whole chain, with the
// warning at each level.
func TestTeamOverrideEscalates(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr")
	f.team("Choir", "staff", "orphan")
	f.teamRule("Band", 50)
	f.teamRule("Choir", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	f.mustSubmit("orphan", "annual", "2026-10-12", "2026-10-14")
	f.events = nil

	r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	if err != nil || !r.Escalate || r.ApproverID != f.people["head"].ID {
		t.Fatalf("override: %+v %v", r, err)
	}
	if len(f.events) != 1 || len(f.events[0].Coverage) != 2 {
		t.Fatalf("submit event coverage = %+v", f.events)
	}
	f.events = nil
	if r, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil || r.ApproverID != f.people["pastor"].ID {
		t.Fatalf("first approval: %+v %v", r, err)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventEscalated || len(f.events[0].Coverage) != 2 {
		t.Fatalf("escalation event coverage = %+v", f.events)
	}
	if r, err = f.svc.Decide(f.ctx, f.people["pastor"], r.ID, true, ""); err != nil || r.Status != StatusApproved {
		t.Fatalf("final approval: %+v %v", r, err)
	}
}

func TestTeamDefaultVersusOwnRule(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr")
	f.team("Choir", "head", "pastor")
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12")
	f.mustSubmit("pastor", "annual", "2026-10-12", "2026-10-12")

	// No rule and no default: no limit.
	f.mustSubmit("staff", "annual", "2026-10-13", "2026-10-13")
	f.mustSubmit("hr", "annual", "2026-10-13", "2026-10-13") // would be 2 of 2 on the 13th
	f.teamDefault(50)
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-12"); !fieldErr(err, "coverage") {
		t.Fatalf("default ignored: %v", err)
	}
	// A team's own rule replaces the default, even a looser one...
	f.teamRule("Band", 100)
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	// ...and the default applies to every other team.
	if _, err := f.submit("head", "annual", "2026-10-12", "2026-10-12"); !fieldErr(err, "coverage") {
		t.Fatalf("default not applied to the other team: %v", err)
	}
	// Removing the default removes the limit.
	f.teamDefault(0)
	f.mustSubmit("head", "annual", "2026-10-12", "2026-10-12")

	if err := f.svc.SetTeamRule(f.ctx, f.teamID("Band"), 101); !fieldErr(err, "percent") {
		t.Fatalf("101%%: %v", err)
	}
	if err := f.svc.SetTeamRule(f.ctx, 9999, 10); err != ErrNotFound {
		t.Fatalf("unknown team: %v", err)
	}
	if err := f.svc.SetTeamDefault(f.ctx, f.people["staff"], 10); err != ErrNotAllowed {
		t.Fatalf("non-admin default: %v", err)
	}
	if err := f.svc.SetTeamDefault(f.ctx, f.people["hr"], 101); !fieldErr(err, "team_default") {
		t.Fatalf("101%% default: %v", err)
	}
}

// Only enabled people with an account here count, for the size and for who is away.
func TestTeamCountsOnlyEnabledStaff(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr", "head")
	f.teams[0].MemberIDs = append(f.teams[0].MemberIDs, "not-staff-1", "not-staff-2") // volunteers with no account
	f.team2()
	f.teamRule("Band", 50)
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")

	// head has left: they neither count towards the size nor could be away.
	if _, err := f.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'head'`); err != nil {
		t.Fatal(err)
	}
	_, err := f.submit("hr", "annual", "2026-10-12", "2026-10-12")
	msg := err.(FieldErrors)["coverage"]
	if !strings.Contains(msg, "100% of the Band team (2 of 2)") {
		t.Fatalf("message = %q", msg)
	}
	teams, _ := f.svc.Teams(f.ctx, false)
	if len(teams) != 1 || teams[0].Size != 2 || teams[0].MaxPercent != 50 {
		t.Fatalf("teams = %+v", teams)
	}
}

// team2 re-syncs the fixture's listing after a test has edited f.teams directly.
func (f *fixture) team2() {
	f.t.Helper()
	if _, _, err := f.us.SyncTeams(f.ctx, "fake", f.teams); err != nil {
		f.t.Fatal(err)
	}
}

func TestDisabledTeamAppliesNothing(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr")
	f.teamRule("Band", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12")
	f.teams = nil
	f.team2() // the group is gone from the source
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	if teams, _ := f.svc.Teams(f.ctx, false); len(teams) != 0 {
		t.Fatalf("active teams = %+v", teams)
	}
	if teams, _ := f.svc.Teams(f.ctx, true); len(teams) != 1 || !teams[0].Disabled || teams[0].MaxPercent != 50 {
		t.Fatalf("all teams = %+v", teams)
	}
}

func TestTeamsFor(t *testing.T) {
	f := newFixture(t)
	f.team("Band", "staff", "hr")
	f.team("Choir", "staff")
	ts, err := f.svc.TeamsFor(f.ctx, f.people["staff"].ID)
	if err != nil || len(ts) != 2 || ts[0].Name != "Band" || ts[1].Name != "Choir" {
		t.Fatalf("teams = %+v, %v", ts, err)
	}
	if ts, _ := f.svc.TeamsFor(f.ctx, f.people["head"].ID); len(ts) != 0 {
		t.Fatalf("head's teams = %+v", ts)
	}
}
