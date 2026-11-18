package handler

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

// The pages for naming a stand-in while away. The rules themselves (dates,
// who may be named, one at a time) are the service's and are tested there; here
// is what the pages and routes do with them.

func (hs *harness) delegationForm(delegate int64, start, end string) url.Values {
	return url.Values{"delegate": {itoa(delegate)}, "start": {start}, "end": {end}, "note": {"on leave"}}
}

func (hs *harness) userID(username string) int64 { return mustUser(hs.t, hs, username).ID }

func (hs *harness) delegationID(delegator string) string {
	hs.t.Helper()
	ds, err := hs.svc.Delegations(hs.t.Context(), leave.DelegationFilter{DelegatorID: hs.userID(delegator)})
	if err != nil || len(ds) == 0 {
		hs.t.Fatalf("no delegation for %s: %v", delegator, err)
	}
	return itoa(ds[0].ID)
}

func TestDelegationsPageSelfService(t *testing.T) {
	hs := newHarness(t)
	ops := hs.signIn("headops")
	status, body := hs.get(ops, "/delegations")
	if status != http.StatusOK || !strings.Contains(body, "Approvals while you are away") ||
		!strings.Contains(body, "You can still decide it yourself") {
		t.Fatalf("page: %d\n%s", status, body)
	}
	sel := regexp.MustCompile(`(?s)<select id="delegate".*?</select>`).FindString(body)
	if !strings.Contains(sel, "Kagiso Kids") || strings.Contains(sel, "Olivia Operations") {
		t.Fatalf("the stand-in select must list others and never the person themselves:\n%s", sel)
	}
	if !strings.Contains(body, `min="2026-10-05"`) {
		t.Fatalf("date inputs have no min of today:\n%s", body)
	}

	res, _ := hs.post(ops, "/delegations", hs.delegationForm(hs.userID("headkids"), "2026-10-05", "2026-10-07"))
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/delegations?notice=delegated&d=") {
		t.Fatalf("create: %d %s", res.StatusCode, loc)
	}
	_, body = hs.get(ops, loc)
	for _, want := range []string{"Kagiso Kids will approve for you from Mon 5 Oct 2026 to Wed 7 Oct 2026. They have been emailed.",
		"<h2>Current and upcoming</h2>", "on leave", `action="/delegations/` + hs.delegationID("headops") + `/revoke"`} {
		if !strings.Contains(body, want) {
			t.Errorf("after saving, the page lacks %q:\n%s", want, body)
		}
	}
	// The notice is built from the stored row, never from the query: a link
	// to someone else's delegation shows the plain notice, not their names.
	_, body = hs.get(hs.signIn("staff1"), "/delegations?notice=delegated&d="+hs.delegationID("headops"))
	if strings.Contains(body, "will approve for") || !strings.Contains(body, "Your stand-in is set.") {
		t.Fatalf("a stranger's link leaked or lost the notice:\n%s", body)
	}
}

func TestDelegationsFormRefusalsRerender(t *testing.T) {
	hs := newHarness(t)
	ops := hs.signIn("headops")
	kids := hs.userID("headkids")
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"past start":      {hs.delegationForm(kids, "2026-10-04", "2026-10-06"), "Start today or later."},
		"thirty-one days": {hs.delegationForm(kids, "2026-10-05", "2026-11-04"), "at most 30 days"},
		"self (tampered)": {hs.delegationForm(hs.userID("headops"), "2026-10-05", "2026-10-06"), leave.MsgDelegateUnusable},
		"no dates":        {hs.delegationForm(kids, "", ""), "Enter the first day."},
		"no stand-in":     {url.Values{"start": {"2026-10-05"}, "end": {"2026-10-06"}}, leave.MsgDelegateUnusable},
	} {
		res, body := hs.post(ops, "/delegations", tc.form)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d, want 422 with %q\n%s", name, res.StatusCode, tc.want, body)
		}
		if strings.Contains(body, `class="error"`) && !strings.Contains(body, `<h1>Approvals while you are away</h1>`) {
			t.Errorf("%s: the form did not come back", name)
		}
	}
	// What was typed comes back in the fields.
	_, body := hs.post(ops, "/delegations", hs.delegationForm(kids, "2026-10-05", "2026-11-04"))
	if !strings.Contains(body, `value="2026-11-04"`) || !strings.Contains(body, `value="`+itoa(kids)+`" selected`) {
		t.Errorf("a refused form lost what was typed:\n%s", body)
	}
	// Exactly thirty days is fine; nothing was saved by the refusals.
	if ds, _ := hs.svc.Delegations(t.Context(), leave.DelegationFilter{}); len(ds) != 0 {
		t.Fatalf("a refusal saved %d delegations", len(ds))
	}
	res, _ := hs.post(ops, "/delegations", hs.delegationForm(kids, "2026-10-05", "2026-11-03"))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("30 days: %d", res.StatusCode)
	}
}

func TestDelegationsRevoke(t *testing.T) {
	hs := newHarness(t)
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")
	id := hs.delegationID("headops")
	// Unknown ids and ones that are not yours are the same 404.
	for name, path := range map[string]string{"bogus": "/delegations/99999/revoke", "not a number": "/delegations/abc/revoke"} {
		if res, _ := hs.post(hs.signIn("headops"), path, url.Values{}); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	for _, who := range []string{"staff1", "headkids", "pastor"} { // the stand-in cannot end it either
		if res, _ := hs.post(hs.signIn(who), "/delegations/"+id+"/revoke", url.Values{}); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s revoking someone else's: %d, want 404", who, res.StatusCode)
		}
	}
	if ds, _ := hs.svc.Delegations(t.Context(), leave.DelegationFilter{DelegatorID: hs.userID("headops")}); len(ds) != 1 {
		t.Fatal("someone else's revoke ended it")
	}
	ops := hs.signIn("headops")
	res, _ := hs.post(ops, "/delegations/"+id+"/revoke", url.Values{})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/delegations?notice=delegation-revoked" {
		t.Fatalf("revoke: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body := hs.get(ops, "/delegations?notice=delegation-revoked")
	if !strings.Contains(body, "Stand-in ended.") || !strings.Contains(body, "No stand-in is set.") ||
		!strings.Contains(body, "ended early by Olivia Operations on Mon 5 Oct 2026") {
		t.Fatalf("after revoking:\n%s", body)
	}
	// A second click says so rather than failing.
	res, _ = hs.post(ops, "/delegations/"+id+"/revoke", url.Values{})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "delegation-over") {
		t.Fatalf("second revoke: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// The link is for people it matters to, and never hidden from anyone in a
// delegation: staff2 approves for nobody and has no reports, so only the
// delegation shows it to them.
func TestDelegationsNavLink(t *testing.T) {
	hs := newHarness(t)
	link := `href="/delegations"`
	has := func(user string) bool {
		_, body := hs.get(hs.signIn(user), "/")
		return strings.Contains(body, link)
	}
	if has("staff2") || has("staff1") {
		t.Fatal("the link shows for staff with nothing to hand over")
	}
	for _, who := range []string{"headops", "hr", "pastor"} { // reports, admin, final approver
		if !has(who) {
			t.Errorf("no link for %s", who)
		}
	}
	hs.delegate("headops", "staff2", "2026-10-05", "2026-10-09")
	if !has("staff2") {
		t.Fatal("a stand-in cannot find the page")
	}
	// Over, it goes again.
	hs.clock(2026, time.October, 10)
	if has("staff2") {
		t.Fatal("the link stays after the stand-in is over")
	}
	// The one asking for a stand-in is in a delegation too.
	hs2 := newHarness(t)
	hs2.delegate("hr", "staff1", "2026-10-05", "2026-10-06")
	if _, body := hs2.get(hs2.signIn("staff1"), "/"); !strings.Contains(body, link) {
		t.Fatal("no link for a delegate of an admin")
	}
}

func TestDelegationsStandInSeesWhatTheyCoverAndRequestsSayWho(t *testing.T) {
	hs := newHarness(t)
	staff, ops := hs.signIn("staff1"), hs.signIn("headops")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")
	kids := hs.signIn("headkids")

	// Request page: the approver row names the stand-in, the stand-in's panel
	// says whose decision it is, and the approver's own panel does not.
	_, body := hs.get(staff, path)
	if !strings.Contains(body, "Kagiso Kids is approving for them until Fri 9 Oct 2026") {
		t.Fatalf("approver row:\n%s", body)
	}
	if _, body = hs.get(kids, path); !strings.Contains(body, "Your decision <small>(for Olivia Operations)</small>") {
		t.Fatalf("stand-in's panel:\n%s", body)
	}
	if _, body = hs.get(ops, path); !strings.Contains(body, "<h2>Your decision</h2>") {
		t.Fatalf("the approver's own panel changed:\n%s", body)
	}
	// Queue: the stand-in's row says for whom; the approver's does not.
	_, body = hs.get(kids, "/approvals")
	if row := rowFor(t, body, path); !strings.Contains(row, "for Olivia Operations") {
		t.Fatalf("queue row lacks 'for':\n%s", row)
	}
	_, body = hs.get(ops, "/approvals")
	if row := rowFor(t, body, path); strings.Contains(row, "for Olivia") {
		t.Fatalf("the approver's own row says 'for':\n%s", row)
	}
	// The stand-in's page lists what they are covering.
	if _, body = hs.get(kids, "/delegations"); !strings.Contains(body, "You are approving for") ||
		!strings.Contains(body, "Olivia Operations, Mon 5 Oct 2026 to Fri 9 Oct 2026") {
		t.Fatalf("no 'You are approving for' section:\n%s", body)
	}
	if _, body = hs.get(ops, "/delegations"); strings.Contains(body, "You are approving for") {
		t.Fatalf("the approver sees a stand-in section:\n%s", body)
	}
	// Decided: Approved by B for A; the line goes once nothing is pending.
	hs.approveAs(kids, path)
	if _, body = hs.get(staff, path); strings.Contains(body, "is approving for them") ||
		!strings.Contains(body, "Kagiso Kids for Olivia Operations") {
		t.Fatalf("after the decision:\n%s", body)
	}
}

// A delegation the requester set up never covers their own leave: hr sets up
// headkids as headops's stand-in, then applies themselves (their approver is
// headops). The page must not advertise a stand-in who could not act, while
// the same delegation is shown on someone else's request to headops.
func TestDelegationsNoStandInLineOnTheCreatorsOwnRequest(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	own := hs.apply(hr, "annual", "2026-10-12", "2026-10-14")
	other := hs.apply(hs.signIn("staff1"), "annual", "2026-10-19", "2026-10-20")
	hr2 := mustUser(t, hs, "hr")
	if _, err := hs.svc.Delegate(t.Context(), hr2, hs.userID("headops"), hs.userID("headkids"), "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	if _, body := hs.get(hr, own); strings.Contains(body, "is approving for them") {
		t.Fatalf("shown on the creator's own request:\n%s", body)
	}
	if _, body := hs.get(hs.signIn("staff1"), other); !strings.Contains(body, "Kagiso Kids is approving for them") {
		t.Fatalf("not shown on someone else's request:\n%s", body)
	}
	if code, _ := hs.get(hs.signIn("headkids"), own); code != http.StatusNotFound {
		t.Fatalf("the stand-in can open the creator's own leave: %d", code)
	}
}

// The stand-in's queue row for someone else's request must not name the person
// who set the delegation up: CanViewNow refuses that person's own leave, so
// "Also away" would be the one place it leaked. Anyone else away still shows.
func TestDelegationsQueueAwayHidesTheCreatorFromTheStandIn(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")
	hs.apply(hs.signIn("hr"), "annual", "2026-10-12", "2026-10-14")
	hs.apply(hs.signIn("headops"), "annual", "2026-10-12", "2026-10-14") // same department as staff1
	hr := mustUser(t, hs, "hr")
	if _, err := hs.svc.Delegate(t.Context(), hr, hs.userID("headops"), hs.userID("headkids"), "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	// The approver is not acting as a stand-in: they still see both.
	_, body := hs.get(hs.signIn("headops"), "/approvals")
	if row := rowFor(t, body, path); !strings.Contains(row, "Hannah HR") || !strings.Contains(row, "Also away") {
		t.Fatalf("the approver's own row lost the away line:\n%s", row)
	}
	_, body = hs.get(hs.signIn("headkids"), "/approvals")
	row := rowFor(t, body, path)
	if !strings.Contains(row, "for Olivia Operations") {
		t.Fatalf("not the stand-in's row:\n%s", row)
	}
	if strings.Contains(row, "Hannah HR") {
		t.Fatalf("the delegation's creator leaked to the stand-in through Also away:\n%s", row)
	}
	if !strings.Contains(row, "Also away: Olivia Operations") {
		t.Fatalf("an ordinary Also away entry went missing:\n%s", row)
	}

	// The filter is for the stand-in's rows only: headkids' own row, held as
	// the normal approver, still names Hannah (she is on the Kids team).
	if _, err := hs.db.Exec(`INSERT INTO team_members (team_id, user_id) VALUES (?, ?)`,
		hs.teamID("Kids team"), hs.staffID("hr")); err != nil {
		t.Fatal(err)
	}
	thandi := hs.apply(hs.signIn("staff2"), "annual", "2026-10-12", "2026-10-14")
	_, body = hs.get(hs.signIn("headkids"), "/approvals")
	own := rowFor(t, body, thandi)
	if strings.Contains(own, "for Olivia Operations") || !strings.Contains(own, "Hannah HR") {
		t.Fatalf("the filter reached a row held as the normal approver:\n%s", own)
	}
}

// An approver who names their own stand-in created the delegation, but their
// own requests never reach the stand-in's queue, so they are not hidden: they
// still show under Also away on the stand-in's rows.
func TestDelegationsQueueAwayKeepsTheApproverWhoSetUpTheirOwnStandIn(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")
	hs.apply(hs.signIn("headops"), "annual", "2026-10-12", "2026-10-14")
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09") // created by headops itself
	_, body := hs.get(hs.signIn("headkids"), "/approvals")
	row := rowFor(t, body, path)
	if !strings.Contains(row, "for Olivia Operations") || !strings.Contains(row, "Also away: Olivia Operations") {
		t.Fatalf("the approver vanished from Also away on their stand-in's row:\n%s", row)
	}
}

// A disabled stand-in cannot decide, so the request page must not say they are
// approving; and the stand-in themself is not told about themself.
func TestDelegationsStandInLineSkipsDisabledAndTheStandInThemself(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")
	if _, body := hs.get(hs.signIn("headkids"), path); strings.Contains(body, "is approving for them") || !strings.Contains(body, "for Olivia Operations") {
		t.Fatalf("the stand-in sees a line about themself, or lost their panel:\n%s", body)
	}
	if _, body := hs.get(staff, path); !strings.Contains(body, "Kagiso Kids is approving for them") {
		t.Fatalf("others lost the line:\n%s", body)
	}
	if _, err := hs.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'headkids'`); err != nil {
		t.Fatal(err)
	}
	if _, body := hs.get(staff, path); strings.Contains(body, "is approving for them") {
		t.Fatalf("a disabled stand-in is shown as approving:\n%s", body)
	}
}

func TestDelegationsAdminPanelAndList(t *testing.T) {
	hs := newHarness(t)
	hr, staff := hs.signIn("hr"), hs.signIn("staff1")
	ops := hs.userID("headops")
	base := "/admin/users/" + itoa(ops)

	_, body := hs.get(hr, base)
	if !strings.Contains(body, "Stand-in for approvals") || !strings.Contains(body, `action="`+base+`/delegations"`) {
		t.Fatalf("person page lacks the panel:\n%s", body)
	}
	sel := regexp.MustCompile(`(?s)<select id="stand-delegate".*?</select>`).FindString(body)
	if !strings.Contains(sel, "Kagiso Kids") || strings.Contains(sel, "Olivia Operations") {
		t.Fatalf("panel select:\n%s", sel)
	}
	// A non-admin cannot, for anyone, and nothing is saved.
	for _, who := range []string{"staff1", "headops"} {
		c := hs.signIn(who)
		res, _ := hs.post(c, base+"/delegations", hs.delegationForm(hs.userID("headkids"), "2026-10-05", "2026-10-06"))
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s creating through the admin route: %d", who, res.StatusCode)
		}
	}
	if ds, _ := hs.svc.Delegations(t.Context(), leave.DelegationFilter{}); len(ds) != 0 {
		t.Fatal("a non-admin created a delegation for someone")
	}
	// A refusal re-renders the person's page with the message.
	res, body := hs.post(hr, base+"/delegations", hs.delegationForm(ops, "2026-10-05", "2026-10-06"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, leave.MsgDelegateUnusable) || !strings.Contains(body, "Stand-in for approvals") {
		t.Fatalf("admin refusal: %d\n%s", res.StatusCode, body)
	}
	res, _ = hs.post(hr, "/admin/users/99999/delegations", hs.delegationForm(ops, "2026-10-05", "2026-10-06"))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown person: %d", res.StatusCode)
	}

	res, _ = hs.post(hr, base+"/delegations", hs.delegationForm(hs.userID("headkids"), "2026-10-05", "2026-10-09"))
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, base+"?notice=delegated&d=") {
		t.Fatalf("admin create: %d %s", res.StatusCode, loc)
	}
	_, body = hs.get(hr, loc)
	if !strings.Contains(body, "Kagiso Kids will approve for Olivia Operations from Mon 5 Oct 2026 to Fri 9 Oct 2026") ||
		!strings.Contains(body, "set up by Hannah HR") {
		t.Fatalf("person page after creating:\n%s", body)
	}
	// The approver sees who set it up on their own page too, and can end it.
	_, body = hs.get(hs.signIn("headops"), "/delegations")
	if !strings.Contains(body, "set up by Hannah HR") {
		t.Fatalf("the approver is not told an admin set it up:\n%s", body)
	}

	_, body = hs.get(hr, "/admin/delegations")
	id := hs.delegationID("headops")
	for _, want := range []string{"Olivia Operations", "Kagiso Kids", "5–9 Oct", "set up by Hannah HR", `action="/admin/delegations/` + id + `/revoke"`} {
		if !strings.Contains(body, want) {
			t.Errorf("/admin/delegations lacks %q:\n%s", want, body)
		}
	}
	if _, body = hs.get(hr, "/admin"); !strings.Contains(body, "<strong>1</strong> stand-in active") {
		t.Fatalf("admin home:\n%s", body)
	}
	if code, _ := hs.get(staff, "/admin/delegations"); code != http.StatusForbidden {
		t.Fatalf("staff on /admin/delegations: %d", code)
	}

	// Revoke from the list and from the person page; staff cannot.
	if res, _ := hs.post(staff, "/admin/delegations/"+id+"/revoke", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("staff revoking through the admin route: %d", res.StatusCode)
	}
	res, _ = hs.post(hr, "/admin/delegations/"+id+"/revoke", url.Values{"from": {"user"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != base+"?notice=delegation-revoked" {
		t.Fatalf("revoke from the person page: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, body = hs.get(hr, "/admin"); !strings.Contains(body, "<strong>0</strong> stand-ins active") {
		t.Fatalf("admin home after revoking:\n%s", body)
	}
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")
	res, _ = hs.post(hr, "/admin/delegations/"+hs.delegationID("headops")+"/revoke", url.Values{})
	if res.Header.Get("Location") != "/admin/delegations?notice=delegation-revoked" {
		t.Fatalf("revoke from the list: %s", res.Header.Get("Location"))
	}
	if res, _ := hs.post(hr, "/admin/delegations/99999/revoke", url.Values{}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus: %d", res.StatusCode)
	}
}

// Every form the stand-in pages render carries a CSRF token. The harness has
// no CSRF layer (main_test and e2e do), so a form that forgot the field would
// otherwise only fail in a browser.
func TestDelegationFormsCarryCSRF(t *testing.T) {
	hs := newHarness(t)
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")
	hr := hs.signIn("hr")
	form := regexp.MustCompile(`(?s)<form[^>]*method="post"[^>]*>.*?</form>`)
	checked := 0
	for who, page := range map[string]string{"headops": "/delegations", "hr": "/admin/delegations",
		"hr ": "/admin/users/" + itoa(hs.userID("headops"))} {
		c := hr
		if who == "headops" {
			c = hs.signIn("headops")
		}
		_, body := hs.get(c, page)
		revokes := 0
		for _, f := range form.FindAllString(body, -1) {
			if strings.Contains(f, "/revoke") || strings.Contains(f, "/delegations") {
				checked++
				if !strings.Contains(f, `name="csrf_token"`) {
					t.Errorf("%s: a form has no CSRF token:\n%s", page, f)
				}
			}
			if strings.Contains(f, "/revoke") {
				revokes++
			}
		}
		if revokes == 0 {
			t.Errorf("%s has no revoke form", page)
		}
	}
	if checked < 6 {
		t.Fatalf("checked only %d forms", checked)
	}
}
