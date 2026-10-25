package handler

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// gridRegion is the month grid's table, so assertions about the grid are not
// satisfied (or spoiled) by the approver list below it.
func gridRegion(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<table class="month-grid">`)
	if i < 0 {
		t.Fatalf("no month grid:\n%s", body)
	}
	region, _, _ := strings.Cut(body[i:], "</table>")
	return region
}

// marked counts the grid cells drawn with a mark ("away" or "pending"), whatever
// day classes follow it.
func marked(g, mark string) int {
	return len(regexp.MustCompile(`<td class="`+mark+`[ "]`).FindAllString(g, -1))
}

// "Today" is Monday 5 October 2026 (the harness clock).

// The grid is the same for a colleague, the approver and an admin: names and
// marked days, never the type. The approver still gets the type in the list
// below, which proves the type was there to leak.
func TestTeamGridNeverShowsType(t *testing.T) {
	hs := newHarness(t)
	head := hs.signIn("headops")
	path := hs.apply(head, "sick", "2026-10-12", "2026-10-13")

	// Pending first: the cells say "awaiting approval", hatched.
	_, body := hs.get(hs.signIn("staff1"), "/team")
	g := gridRegion(t, body)
	if !strings.Contains(g, "Olivia Operations") || marked(g, "pending") != 2 || !strings.Contains(g, "awaiting approval") {
		t.Fatalf("colleague's grid lacks the person or the pending cells:\n%s", g)
	}

	pastor := hs.signIn("pastor")
	hs.approveAs(pastor, path)
	for _, who := range []string{"staff1", "pastor", "hr"} {
		code, body := hs.get(hs.signIn(who), "/team")
		g := gridRegion(t, body)
		if code != 200 || !strings.Contains(g, "Olivia Operations") || marked(g, "away") != 2 ||
			!strings.Contains(g, ">away<") {
			t.Errorf("%s: grid lacks the person or the two away cells:\n%s", who, g)
		}
		if strings.Contains(strings.ToLower(g), "sick") {
			t.Errorf("%s: the grid names the leave type:\n%s", who, g)
		}
	}
	// The approver's list does carry it.
	_, body = hs.get(pastor, "/team")
	list := body[strings.Index(body, "Leave of the people you approve for"):]
	if !strings.Contains(list, "Sick leave") {
		t.Errorf("approver list lacks the type:\n%s", list)
	}
	// A colleague gets no such list.
	_, body = hs.get(hs.signIn("staff1"), "/team")
	if strings.Contains(body, "Leave of the people you approve for") || strings.Contains(body, "Sick leave") {
		t.Errorf("colleague sees the approver list or the type:\n%s", body)
	}
}

func TestTeamMonthParam(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	_, body := hs.get(c, "/team")
	if !strings.Contains(body, "Who is away in October 2026") || !strings.Contains(body, `href="/team?month=2026-09"`) ||
		!strings.Contains(body, `href="/team?month=2026-11"`) || strings.Contains(body, "This month") {
		t.Errorf("default month:\n%s", body)
	}
	_, body = hs.get(c, "/team?month=2026-11")
	if !strings.Contains(body, "Who is away in November 2026") || !strings.Contains(body, `href="/team?month=2026-10"`) ||
		!strings.Contains(body, `href="/team?month=2026-12"`) || !strings.Contains(body, `href="/team">This month`) {
		t.Errorf("november:\n%s", body)
	}
	// Garbage is the current month and is never echoed.
	for _, bad := range []string{"zzz", "2026-13", "<script>alert(1)</script>", "2026-1", ""} {
		_, body = hs.get(c, "/team?month="+url.QueryEscape(bad))
		// (The theme form elsewhere on the page carries the URL, escaped; the
		// grid's own caption and links must not.)
		nav, _, _ := strings.Cut(body[strings.Index(body, `<nav class="month-nav"`):], "</nav>")
		if !strings.Contains(body, "Who is away in October 2026") || strings.Contains(nav, "zzz") || strings.Contains(nav, "script") {
			t.Errorf("month=%q:\n%s", bad, body)
		}
	}
	// Out of range is clamped to the edge, and the edge has no further link.
	_, body = hs.get(c, "/team?month=2020-01")
	if !strings.Contains(body, "Who is away in October 2025") || strings.Contains(body, "month=2025-09") || !strings.Contains(body, "month=2025-11") {
		t.Errorf("past edge:\n%s", body)
	}
	_, body = hs.get(c, "/team?month=2099-01")
	if !strings.Contains(body, "Who is away in October 2028") || strings.Contains(body, "month=2028-11") || !strings.Contains(body, "month=2028-09") {
		t.Errorf("future edge:\n%s", body)
	}
}

// 31 day columns and the day classes: weekends and the holiday shaded and
// named, today highlighted.
func TestTeamGridDayClasses(t *testing.T) {
	hs := newHarness(t)
	hs.apply(hs.signIn("staff1"), "annual", "2026-09-22", "2026-09-25")
	_, body := hs.get(hs.signIn("hr"), "/team?month=2026-09")
	g := gridRegion(t, body)
	for _, want := range []string{
		`<th scope="col" class="holiday" title="Thu 24 Sep: Heritage Day">`,
		`<th scope="col" class="off" title="Sat 5 Sep">`,
		`<th scope="col" class="" title="Mon 7 Sep">`,
		`<td class="pending holiday" title="Heritage Day">`,
	} {
		if !strings.Contains(g, want) {
			t.Errorf("grid lacks %s:\n%s", want, g)
		}
	}
	if n := strings.Count(g, `<th scope="col"`); n != 31 {
		// 30 days and the "Who" corner.
		t.Errorf("%d column headers in September, want 31", n)
	}
	if n := marked(g, "pending"); n != 4 {
		t.Errorf("%d pending cells, want 4 (22 to 25 Sep)", n)
	}
	// Today is highlighted in its own month only.
	_, body = hs.get(hs.signIn("hr"), "/team")
	if !strings.Contains(gridRegion(t, body), `<th scope="col" class="today" title="Mon 5 Oct">`) {
		t.Errorf("today not marked:\n%s", body)
	}
	if strings.Contains(g, "today") {
		t.Error("today marked in September")
	}
	// A weekend in October is shaded in the leave row too.
	hs.apply(hs.signIn("staff1"), "annual", "2026-10-09", "2026-10-12")
	_, body = hs.get(hs.signIn("hr"), "/team")
	if g := gridRegion(t, body); !strings.Contains(g, `<td class="pending off">`) {
		t.Errorf("weekend cell inside leave not shaded:\n%s", g)
	}
}

func TestTeamGridOwnRow(t *testing.T) {
	hs := newHarness(t)
	hs.apply(hs.signIn("headops"), "annual", "2026-10-07", "2026-10-07")
	staff := hs.signIn("staff1")
	hs.apply(staff, "annual", "2026-10-06", "2026-10-07")
	_, body := hs.get(staff, "/team")
	g := gridRegion(t, body)
	you, olivia := strings.Index(g, `<th scope="row" title="Sam Staff">You</th>`), strings.Index(g, "Olivia Operations")
	if you < 0 || olivia < 0 || you > olivia {
		t.Fatalf("You row missing or not first (You at %d, Olivia at %d):\n%s", you, olivia, g)
	}
	if strings.Contains(g, ">Sam Staff<") {
		t.Errorf("own name shown instead of You:\n%s", g)
	}
	yours := g[you:olivia]
	if marked(yours, "pending") != 2 {
		t.Errorf("own pending cells:\n%s", yours)
	}
	// Approved leave is marked away.
	hs.approveAs(hs.signIn("headops"), hs.apply(staff, "annual", "2026-10-20", "2026-10-20"))
	_, body = hs.get(staff, "/team")
	if g := gridRegion(t, body); marked(g, "away") < 1 {
		t.Errorf("own approved leave not marked:\n%s", g)
	}
}

func TestTeamGridEmptyAndScope(t *testing.T) {
	hs := newHarness(t)
	// Thandi (Kids, Kids team) shares no department or team with Sam.
	hs.apply(hs.signIn("staff2"), "annual", "2026-10-13", "2026-10-13")
	staff := hs.signIn("staff1")
	code, body := hs.get(staff, "/team")
	g := gridRegion(t, body)
	if code != 200 || !strings.Contains(body, "Nobody you work with is away this month.") || strings.Contains(g, "Thandi") ||
		!strings.Contains(g, "<caption") || strings.Count(g, `<th scope="col"`) != 32 { // 31 days and the "Who" corner
		t.Errorf("empty state, header or scope:\n%s", body)
	}
	// The same leave is in Thandi's own grid, as "You".
	_, body = hs.get(hs.signIn("staff2"), "/team")
	if g := gridRegion(t, body); !strings.Contains(g, ">You<") || strings.Contains(body, "Nobody you work with") {
		t.Errorf("own row:\n%s", g)
	}
}

func TestTeamPageNoInlineStyleOrHandlers(t *testing.T) {
	hs := newHarness(t)
	hs.apply(hs.signIn("headops"), "annual", "2026-10-07", "2026-10-09")
	bad := regexp.MustCompile(`(?i)\sstyle\s*=|\son[a-z]+\s*=`)
	_, body := hs.get(hs.signIn("pastor"), "/team")
	if loc := bad.FindStringIndex(body); loc != nil {
		t.Errorf("inline style or handler near %q", body[loc[0]:min(loc[1]+20, len(body))])
	}
}

// Navigation crosses the year both ways, and February 2028 is a leap month.
func TestTeamMonthEdgesAndLeapYear(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	_, body := hs.get(c, "/team?month=2026-12")
	if !strings.Contains(body, `href="/team?month=2027-01"`) || !strings.Contains(body, `href="/team?month=2026-11"`) {
		t.Errorf("december links:\n%s", body)
	}
	_, body = hs.get(c, "/team?month=2027-01")
	if !strings.Contains(body, "Who is away in January 2027") || !strings.Contains(body, `href="/team?month=2026-12"`) ||
		!strings.Contains(body, `href="/team?month=2027-02"`) {
		t.Errorf("january links:\n%s", body)
	}
	_, body = hs.get(c, "/team?month=2028-02")
	if n := strings.Count(gridRegion(t, body), `<th scope="col"`); n != 30 { // 29 days and "Who"
		t.Errorf("%d column headers in Feb 2028, want 30", n)
	}
	_, body = hs.get(c, "/team?month=2027-02")
	if n := strings.Count(gridRegion(t, body), `<th scope="col"`); n != 29 {
		t.Errorf("%d column headers in Feb 2027, want 29", n)
	}
}

// Where ranges overlap, approved is drawn and said; adjacent ranges are
// simply both marked; no class carries stray spaces.
func TestTeamGridOverlapAndAdjacent(t *testing.T) {
	hs := newHarness(t)
	head := hs.signIn("headops")
	hs.apply(head, "annual", "2026-10-21", "2026-10-22") // pending, adjacent to the next
	hs.apply(head, "annual", "2026-10-23", "2026-10-23")
	// Pending 12-16 Oct with an approved 14 Oct inside it: the approver
	// cannot book overlapping leave through the pages, so seed the second
	// range directly.
	hs.apply(head, "annual", "2026-10-12", "2026-10-16")
	if _, err := hs.db.ExecContext(context.Background(), `INSERT INTO requests(user_id, type_id, kind, start_date, end_date, days, cycle_start, status, approver_id, created_at)
		SELECT user_id, type_id, kind, '2026-10-14', '2026-10-14', 1, cycle_start, 'approved', approver_id, created_at FROM requests WHERE start_date = '2026-10-12'`); err != nil {
		t.Fatal(err)
	}
	_, body := hs.get(hs.signIn("staff1"), "/team")
	g := gridRegion(t, body)
	if marked(g, "away") != 1 || marked(g, "pending") != 4+3 {
		t.Errorf("overlap: %d away, %d pending cells, want 1 and 7 (12,13,15,16 and 21,22,23):\n%s", marked(g, "away"), marked(g, "pending"), g)
	}
	if strings.Contains(g, `class="away pending`) || regexp.MustCompile(`class="[^"]* "|class=" `).MatchString(g) {
		t.Errorf("stray space or two marks in a class:\n%s", g)
	}
	if n := strings.Count(g, ">awaiting approval<"); n != 7 || strings.Count(g, ">away<") != 1 {
		t.Errorf("hidden text does not match the marks: %d awaiting", n)
	}
}

// An approver with nothing to list still gets the section, with a line saying
// so; someone who approves nobody gets no section.
func TestTeamApproverListEmptyAndAbsent(t *testing.T) {
	hs := newHarness(t)
	_, body := hs.get(hs.signIn("headops"), "/team")
	if !strings.Contains(body, "Leave of the people you approve for") || !strings.Contains(body, "Nobody you approve for has leave coming up.") {
		t.Errorf("approver with nothing coming:\n%s", body)
	}
	_, body = hs.get(hs.signIn("staff1"), "/team")
	if strings.Contains(body, "Leave of the people you approve for") || strings.Contains(body, "Nobody you approve for") {
		t.Errorf("non-approver sees the section:\n%s", body)
	}
}
