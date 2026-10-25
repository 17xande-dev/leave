package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// rowFor is the queue's table row for a request, so assertions about one row
// are not satisfied by another's text.
func rowFor(t *testing.T, body, path string) string {
	t.Helper()
	i := strings.Index(body, `<a href="`+path+`?from=approvals">`)
	if i < 0 {
		t.Fatalf("no queue row for %s:\n%s", path, body)
	}
	start := strings.LastIndex(body[:i], "<tr>")
	end := strings.Index(body[i:], "</tr>")
	return body[start : i+end]
}

func mustUser(t *testing.T, hs *harness, username string) users.User {
	t.Helper()
	u, err := hs.us.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func idOf(path string) string { return strings.TrimPrefix(path, "/leave/") }

func (hs *harness) status(path string) string {
	hs.t.Helper()
	id, _ := strconv.ParseInt(idOf(path), 10, 64)
	r, err := hs.svc.Get(context.Background(), id)
	if err != nil {
		hs.t.Fatal(err)
	}
	return r.Status
}

func TestQueueBalanceAfter(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	three := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	next := hs.apply(staff, "annual", "2027-01-04", "2027-01-05")
	unpaid := hs.apply(staff, "unpaid", "2026-11-02", "2026-11-03")
	ty, _ := hs.svc.Type(context.Background(), mustAtoi(t, hs.anyTypeID("weekend")))
	ty.Active, ty.Allowance = true, new(int)
	*ty.Allowance = 4
	if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
		t.Fatal(err)
	}
	wk := hs.apply(hs.signIn("hr"), "weekend", "2026-10-17", "2026-10-18")

	_, body := hs.get(head, "/approvals")
	// The three days are already reserved: 15 - 3 is 12, not 9.
	for path, want := range map[string]string{
		three:  "12 working days left in this cycle afterwards",
		next:   "13 working days left in 2027 afterwards",
		wk:     "3 weekends left in this cycle afterwards",
		unpaid: "",
	} {
		row := rowFor(t, body, path)
		if want == "" {
			if strings.Contains(row, "afterwards") {
				t.Errorf("unlimited type has a balance line:\n%s", row)
			}
		} else if !strings.Contains(row, want) {
			t.Errorf("%s lacks %q:\n%s", path, want, row)
		}
	}

	// Below zero, said as such.
	if err := hs.svc.AddAdjustment(context.Background(), mustUser(t, hs, "hr"), leave.Adjustment{
		UserID: hs.staffID("staff1"), TypeID: mustAtoi(t, hs.typeID("annual")), CycleStart: "2026-01-01", Days: -14, Reason: "t"}); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(head, "/approvals")
	if row := rowFor(t, body, three); !strings.Contains(row, "2 working days below zero afterwards") {
		t.Errorf("below zero:\n%s", row)
	}
}

// Puts headops on the Kids team, so Thandi (whose approver is headkids) is
// their colleague and not their report.
func (hs *harness) headopsJoinsKids() {
	hs.t.Helper()
	if _, err := hs.db.Exec(`INSERT INTO team_members (team_id, user_id)
		VALUES (?, ?)`, hs.teamID("Kids team"), hs.staffID("headops")); err != nil {
		hs.t.Fatal(err)
	}
}

func TestQueueAlsoAway(t *testing.T) {
	hs := newHarness(t)
	staff, head, kids, pastor := hs.signIn("staff1"), hs.signIn("headops"), hs.signIn("headkids"), hs.signIn("pastor")

	// Sam (headops's report, Operations) and Thandi (Kids) are off sick;
	// Kagiso (Kids, the pastor's report) is on annual leave.
	hs.approveAs(head, hs.apply(staff, "sick", "2026-10-12", "2026-10-13"))
	hs.approveAs(kids, hs.apply(hs.signIn("staff2"), "sick", "2026-10-13", "2026-10-14"))
	hs.approveAs(pastor, hs.apply(kids, "annual", "2026-10-13", "2026-10-14"))
	hannah := hs.apply(hs.signIn("hr"), "annual", "2026-10-12", "2026-10-14")
	olivia := hs.apply(head, "annual", "2026-10-12", "2026-10-14")

	// The scope is the requester's department: Hannah's row lists Sam, with the
	// type because he reports to the approver, and neither of the Kids people.
	_, body := hs.get(head, "/approvals")
	row := rowFor(t, body, hannah)
	if !strings.Contains(row, "Sam Staff, Sick leave (12–13 Oct)</small>") ||
		strings.Contains(row, "Thandi") || strings.Contains(row, "Kagiso") {
		t.Errorf("Hannah's row:\n%s", row)
	}
	// The pastor approves Olivia's: Sam is listed (her department) without the
	// type, which is not the pastor's to see; Kagiso is another department.
	_, body = hs.get(pastor, "/approvals")
	row = rowFor(t, body, olivia)
	if !strings.Contains(row, "Sam Staff (12–13 Oct)</small>") || strings.Contains(row, "Sick") ||
		strings.Contains(row, "Kagiso") || strings.Contains(row, "Thandi") {
		t.Errorf("Olivia's row for the pastor:\n%s", row)
	}

	// Sharing a team brings people in from other departments; the type still
	// follows the viewer: Kagiso is the pastor's report, Thandi is not.
	if _, err := hs.db.Exec(`INSERT INTO team_members (team_id, user_id) VALUES (?, ?)`,
		hs.teamID("Kids team"), hs.staffID("headops")); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(pastor, "/approvals")
	row = rowFor(t, body, olivia)
	if !strings.Contains(row, "Kagiso Kids, Annual leave (13–14 Oct)") || !strings.Contains(row, "Thandi Teacher (13–14 Oct)") ||
		strings.Contains(row, "Sick") {
		t.Errorf("Olivia's row after joining Kids team:\n%s", row)
	}

	// The requester is left out: Sam's own cancellation request must not list
	// Sam's own approved leave.
	id, _ := strconv.ParseInt(idOf(hs.apply(staff, "annual", "2026-11-09", "2026-11-10")), 10, 64)
	c := mustUser(t, hs, "staff1")
	if _, err := hs.svc.Decide(context.Background(), mustUser(t, hs, "headops"), id, true, ""); err != nil {
		t.Fatal(err)
	}
	cancel, err := hs.svc.RequestCancellation(context.Background(), c, id, "no")
	if err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(head, "/approvals")
	row = rowFor(t, body, fmt.Sprintf("/leave/%d", cancel.ID))
	if strings.Contains(row, "Also away") || !strings.Contains(row, "Cancel: Annual leave") ||
		!strings.Contains(row, `aria-label="Select Cancel: Sam Staff`) {
		t.Errorf("cancellation row:\n%s", row)
	}
}

// groupAway with withType false is what keeps the apply hint type-free.
func TestGroupAwayBlanksTheTypeUnlessAsked(t *testing.T) {
	away := []leave.Away{{UserID: 1, Name: "A", Start: "2026-10-12", End: "2026-10-12", LeaveType: "Sick leave"},
		{UserID: 2, Name: "S", Start: "2026-10-12", End: "2026-10-12", LeaveType: "Sick leave"}}
	if g := groupAway(away, false, 2); len(g) != 1 || g[0].Ranges[0].Type != "" {
		t.Errorf("withType false: %+v", g)
	}
	if g := groupAway(away, true, 0); len(g) != 2 || g[0].Ranges[0].Type != "Sick leave" {
		t.Errorf("withType true: %+v", g)
	}
}

// Two pending requests of one type are both counted in what is left, so the
// row cannot say "afterwards" for either.
func TestQueueSharedBalanceWording(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	a := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	_, body := hs.get(head, "/approvals")
	if row := rowFor(t, body, a); !strings.Contains(row, "12 working days left in this cycle afterwards") {
		t.Errorf("one pending:\n%s", row)
	}
	b := hs.apply(staff, "annual", "2026-11-02", "2026-11-06")
	_, body = hs.get(head, "/approvals")
	for _, p := range []string{a, b} {
		if row := rowFor(t, body, p); !strings.Contains(row, "7 working days left in this cycle if all their pending requests are approved") ||
			strings.Contains(row, "afterwards") {
			t.Errorf("two pending, %s:\n%s", p, row)
		}
	}
}

// The escalation check is inside Decide's transaction: a request that became
// escalated after the page read it is still refused, and does not move.
func TestBulkRefusesARequestEscalatedAfterTheRead(t *testing.T) {
	hs := newHarness(t)
	p := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")
	id := mustAtoi(t, idOf(p))
	if _, err := hs.db.Exec(`UPDATE requests SET escalate = 1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	_, err := hs.svc.DecideOrdinary(context.Background(), mustUser(t, hs, "headops"), id)
	if !errors.Is(err, leave.ErrNeedsOverride) {
		t.Fatalf("DecideOrdinary on an escalated request: %v", err)
	}
	if r, _ := hs.svc.Get(context.Background(), id); r.Status != "pending" || r.ApproverID != hs.staffID("headops") {
		t.Errorf("request moved: %s approver %d", r.Status, r.ApproverID)
	}
}

func TestQueueAwayIsCapped(t *testing.T) {
	var away []leave.Away
	for i := 1; i <= 7; i++ {
		away = append(away, leave.Away{UserID: int64(i), Name: fmt.Sprintf("P%d", i), Start: "2026-10-12", End: "2026-10-12", Status: "approved"})
	}
	away = append(away, leave.Away{UserID: 99, Name: "Self", Start: "2026-10-12", End: "2026-10-12"})
	got, more := queueAway(away, 99)
	if len(got) != 5 || more != 2 || got[0].Name != "P1" {
		t.Errorf("got %d shown, %d more", len(got), more)
	}
}

func TestBulkApprove(t *testing.T) {
	hs := newHarness(t)
	staff, hr, head := hs.signIn("staff1"), hs.signIn("hr"), hs.signIn("headops")
	a := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	b := hs.apply(hr, "annual", "2026-10-19", "2026-10-20")

	_, body := hs.get(head, "/approvals")
	for _, p := range []string{a, b} {
		if !strings.Contains(rowFor(t, body, p), `name="id" value="`+idOf(p)+`"`) {
			t.Fatalf("no checkbox on %s", p)
		}
	}
	if !strings.Contains(body, `<button type="submit" form="bulk">Approve selected</button>`) {
		t.Fatal("no Approve selected button")
	}

	// Repeated ids are one approval.
	res, _ := hs.post(head, "/approvals/approve", url.Values{"id": {idOf(a), idOf(b), idOf(a)}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/approvals?notice=approved-many" {
		t.Fatalf("bulk approve: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if hs.status(a) != "approved" || hs.status(b) != "approved" {
		t.Error("not both approved")
	}
	if _, body := hs.get(staff, "/"); !strings.Contains(body, `<td class="num">12</td>`) {
		t.Errorf("balance not updated:\n%s", body)
	}
	if _, body := hs.get(head, "/approvals?notice=approved-many"); !strings.Contains(body, "The selected requests are approved.") ||
		!strings.Contains(body, "Nothing is waiting for you.") {
		t.Errorf("after bulk approve:\n%s", body)
	}
}

func TestBulkApproveRefusesWhatIsNotYours(t *testing.T) {
	hs := newHarness(t)
	head := hs.signIn("headops")
	mine := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")
	others := hs.apply(hs.signIn("staff2"), "annual", "2026-10-12", "2026-10-14") // headkids's to decide
	own := hs.apply(head, "annual", "2026-10-26", "2026-10-27")                   // pastor's to decide

	res, body := hs.post(head, "/approvals/approve", url.Values{"id": {idOf(mine), idOf(others), idOf(own), "9999"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", res.StatusCode)
	}
	if hs.status(mine) != "approved" || hs.status(others) != "pending" || hs.status(own) != "pending" {
		t.Errorf("statuses: %s %s %s", hs.status(mine), hs.status(others), hs.status(own))
	}
	for _, id := range []string{idOf(others), idOf(own), "9999"} {
		if !strings.Contains(body, fmt.Sprintf("Request %s: you cannot approve it.", id)) {
			t.Errorf("failure for %s not reported:\n%s", id, body)
		}
	}
	// Nothing about the other approver's request leaks, and the one approved is counted.
	if strings.Contains(body, "Thandi") || !strings.Contains(body, "1 approved.") {
		t.Errorf("leak or missing count:\n%s", body)
	}

	// A malformed id is a 400, and changes nothing.
	for _, bad := range []string{"x", "-1", "0", ""} {
		res, _ := hs.post(head, "/approvals/approve", url.Values{"id": {bad}})
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q: %d, want 400", bad, res.StatusCode)
		}
	}
	// Nothing ticked is said, not a silent success.
	if res, body := hs.post(head, "/approvals/approve", url.Values{}); res.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "Tick at least one request first.") {
		t.Errorf("nothing ticked: %d\n%s", res.StatusCode, body)
	}
	// And too many.
	many := url.Values{}
	for i := 1; i <= bulkMax+1; i++ {
		many.Add("id", strconv.Itoa(i))
	}
	if res, _ := hs.post(head, "/approvals/approve", many); res.StatusCode != http.StatusBadRequest {
		t.Errorf("over the cap: %d", res.StatusCode)
	}
}

// Decide would take a bulk approval of an escalated request as an override, so
// the bulk handler refuses it, and the page offers no checkbox.
func TestBulkApproveNeverOverrides(t *testing.T) {
	hs := newHarness(t)
	hs.limitOperations()
	head := hs.signIn("headops")
	hs.apply(head, "annual", "2026-10-12", "2026-10-14")
	res, _ := hs.post(hs.signIn("staff1"), "/leave", hs.form("annual", "2026-10-12", "2026-10-14", url.Values{"override": {"1"}}))
	special, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	plain := hs.apply(hs.signIn("hr"), "annual", "2026-10-20", "2026-10-21")

	_, body := hs.get(head, "/approvals")
	if strings.Contains(rowFor(t, body, special), `type="checkbox"`) || !strings.Contains(rowFor(t, body, special), "Override and send to") {
		t.Errorf("escalated row:\n%s", rowFor(t, body, special))
	}
	if !strings.Contains(rowFor(t, body, plain), `type="checkbox"`) {
		t.Error("ordinary row has no checkbox")
	}

	res, body = hs.post(head, "/approvals/approve", url.Values{"id": {idOf(special), idOf(plain)}})
	if res.StatusCode != http.StatusUnprocessableEntity || hs.status(plain) != "approved" || hs.status(special) != "pending" {
		t.Fatalf("%d, plain %s, special %s", res.StatusCode, hs.status(plain), hs.status(special))
	}
	if !strings.Contains(body, "Sam Staff, Annual leave (12–14 Oct): needs special permission.") {
		t.Errorf("failure not named:\n%s", body)
	}
	// Only the escalated row is left: nothing to tick, so no bulk button.
	if strings.Contains(body, "Approve selected") {
		t.Error("Approve selected shown with nothing tickable")
	}
	// No step was logged: it did not move up.
	if r, _ := hs.svc.Get(context.Background(), mustAtoi(t, idOf(special))); r.ApproverID != hs.staffID("headops") {
		t.Errorf("approver moved to %d", r.ApproverID)
	}
}

func TestDeclineFromTheRow(t *testing.T) {
	hs := newHarness(t)
	head := hs.signIn("headops")
	p := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")

	_, body := hs.get(head, "/approvals")
	if row := rowFor(t, body, p); !strings.Contains(row, `<details class="decline">`) || !strings.Contains(row, `name="from" value="queue"`) ||
		!strings.Contains(row, `name="decision" value="decline"`) || !strings.Contains(row, "Details</a>") {
		t.Fatalf("no decline form in the row:\n%s", row)
	}

	// A browser would stop the empty box; a forged post gets the queue back,
	// the row's details open, the typed text kept and the reason said.
	res, body := hs.post(head, p+"/decide", url.Values{"decision": {"decline"}, "from": {"queue"}, "note": {"  "}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `<details class="decline" open>`) ||
		!strings.Contains(body, "Say why") || !strings.Contains(body, "<h1>Approvals</h1>") || hs.status(p) != "pending" {
		t.Fatalf("no note: %d\n%s", res.StatusCode, body)
	}

	res, _ = hs.post(head, p+"/decide", url.Values{"decision": {"decline"}, "from": {"queue"}, "note": {"clashes"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/approvals?notice=declined" || hs.status(p) != "declined" {
		t.Fatalf("with note: %d %s %s", res.StatusCode, res.Header.Get("Location"), hs.status(p))
	}
}
