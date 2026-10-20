package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestTeamsAdminPage(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	_, body := hs.get(hr, "/admin/teams")
	for _, want := range []string{"Teams", "Band", "Kids team", "Default limit"} {
		if !strings.Contains(body, want) {
			t.Fatalf("teams page lacks %q:\n%s", want, body)
		}
	}
	ctx := t.Context()
	band := itoa(hs.teamID("Band"))
	save := func(id, pct string) *http.Response {
		res, _ := hs.post(hr, "/admin/teams", url.Values{"team": {id}, "percent": {pct}})
		return res
	}
	if res := save(band, "40"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", res.StatusCode)
	}
	teams, _ := hs.svc.Teams(ctx, false)
	if teams[0].Name != "Band" || teams[0].MaxPercent != 40 || teams[0].Size != 3 {
		t.Fatalf("teams = %+v", teams)
	}
	// Strict: whole numbers 1 to 100, or empty.
	for _, bad := range []string{"abc", "0", "101", "+5", "-5", "4.5", "1e1", "1000"} {
		if res := save(band, bad); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("percent %q: %d, want 422", bad, res.StatusCode)
		}
	}
	for _, id := range []string{"99999", "abc", "0", ""} {
		if res := save(id, "10"); res.StatusCode != http.StatusBadRequest {
			t.Errorf("team %q: %d, want 400", id, res.StatusCode)
		}
	}
	if teams, _ = hs.svc.Teams(ctx, false); teams[0].MaxPercent != 40 {
		t.Fatalf("a refused save changed the rule: %+v", teams)
	}
	if res := save(band, ""); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	if teams, _ = hs.svc.Teams(ctx, false); teams[0].MaxPercent != 0 {
		t.Fatalf("rule not removed: %+v", teams)
	}

	// The default, here and on the settings page, is one value.
	if res, _ := hs.post(hr, "/admin/teams/default", url.Values{"percent": {" 30 "}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("default: %d", res.StatusCode)
	}
	if st, _ := hs.svc.Settings(ctx); st.TeamDefaultPercent != 30 {
		t.Fatalf("default = %d", st.TeamDefaultPercent)
	}
	for _, bad := range []string{"x", "0", "101", "-1"} {
		if res, _ := hs.post(hr, "/admin/teams/default", url.Values{"percent": {bad}}); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("default %q: %d, want 422", bad, res.StatusCode)
		}
	}
	if _, body := hs.get(hr, "/admin/settings"); !strings.Contains(body, `name="team_default" inputmode="numeric" size="4" value="30"`) {
		t.Fatalf("settings page lacks the default:\n%s", body)
	}
	if res, _ := hs.post(hr, "/admin/teams/default", url.Values{"percent": {""}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear default: %d", res.StatusCode)
	}
	if st, _ := hs.svc.Settings(ctx); st.TeamDefaultPercent != 0 {
		t.Fatalf("default not cleared: %d", st.TeamDefaultPercent)
	}
}

func (hs *harness) teamID(name string) int64 {
	hs.t.Helper()
	ts, err := hs.svc.Teams(hs.t.Context(), true)
	if err != nil {
		hs.t.Fatal(err)
	}
	for _, tm := range ts {
		if tm.Name == name {
			return tm.ID
		}
	}
	hs.t.Fatalf("no team %s", name)
	return 0
}

func TestSettingsSavesTeamDefault(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	form := url.Values{"day": {"mon", "tue", "wed", "thu", "fri"}, "restricted_day": {""}, "restricted_min": {"3"},
		"sick_backdate": {"14"}, "sick_cycle": {"2025"}, "team_default": {"45"}}
	if res, body := hs.post(hr, "/admin/settings", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", res.StatusCode, body)
	}
	if st, _ := hs.svc.Settings(t.Context()); st.TeamDefaultPercent != 45 {
		t.Fatalf("default = %d", st.TeamDefaultPercent)
	}
	for _, bad := range []string{"abc", "0", "101", "-5"} {
		form.Set("team_default", bad)
		if res, _ := hs.post(hr, "/admin/settings", form); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("team_default %q: %d, want 422", bad, res.StatusCode)
		}
	}
	form.Set("team_default", "")
	hs.post(hr, "/admin/settings", form)
	if st, _ := hs.svc.Settings(t.Context()); st.TeamDefaultPercent != 0 {
		t.Fatalf("not cleared: %d", st.TeamDefaultPercent)
	}
}

func TestTeamsPageSaysWhenOff(t *testing.T) {
	hs := newHarness(t)
	hs.h.teamsOn = false
	_, body := hs.get(hs.signIn("hr"), "/admin/teams")
	if !strings.Contains(body, "Teams are not set up") || !strings.Contains(body, "LDAP_TEAMS_BASE_DN") || strings.Contains(body, "Kids team") {
		t.Fatalf("off page:\n%s", body)
	}
}

func TestInactiveTeamsHiddenUntilAsked(t *testing.T) {
	hs := newHarness(t)
	if _, _, err := hs.us.SyncTeams(t.Context(), "fake", nil); err != nil {
		t.Fatal(err)
	}
	hr := hs.signIn("hr")
	_, body := hs.get(hr, "/admin/teams")
	if strings.Contains(body, "Kids team") || !strings.Contains(body, "Show inactive teams") {
		t.Fatalf("default view:\n%s", body)
	}
	_, body = hs.get(hr, "/admin/teams?inactive=1")
	if !strings.Contains(body, "Kids team") || !strings.Contains(body, "gone from the directory") {
		t.Fatalf("inactive view:\n%s", body)
	}
}

func TestPersonPageListsTheirTeams(t *testing.T) {
	hs := newHarness(t)
	staff, _ := hs.us.ByUsername(t.Context(), "staff2")
	_, body := hs.get(hs.signIn("hr"), "/admin/users/"+itoa(staff.ID))
	if !strings.Contains(body, "Kids team") {
		t.Fatalf("person page lacks the team:\n%s", body)
	}
	if strings.Contains(body, "Band") {
		t.Fatalf("person page lists a team they are not in:\n%s", body)
	}
}

// staff1 and headops share the Band; a limit on it blocks the second person
// and the warning names the team, with the same special-permission path.
func TestTeamLimitOnTheApplyPage(t *testing.T) {
	hs := newHarness(t)
	hs.svc.SetTeamRule(t.Context(), hs.teamID("Band"), 34)
	hs.apply(hs.signIn("headops"), "annual", "2026-10-12", "2026-10-14")
	staff := hs.signIn("staff1")

	res, body := hs.post(staff, "/leave", hs.form("annual", "2026-10-12", "2026-10-14", nil))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("over the team limit: %d", res.StatusCode)
	}
	for _, want := range []string{"67% of the Band team (2 of 3) would be away on Mon 12 Oct. The limit is 34%.", "Request special permission"} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply page lacks %q:\n%s", want, body)
		}
	}
	res, _ = hs.post(staff, "/leave", hs.form("annual", "2026-10-12", "2026-10-14", url.Values{"override": {"1"}}))
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	if _, body := hs.get(staff, path); !strings.Contains(body, "needs special permission") || !strings.Contains(body, "the Band team") {
		t.Fatalf("request page:\n%s", body)
	}
	// The head of operations (the approver) sees the warning in the queue.
	if _, body := hs.get(hs.signIn("headops"), "/approvals"); !strings.Contains(body, "the Band team") {
		t.Fatalf("approvals queue:\n%s", body)
	}
}
