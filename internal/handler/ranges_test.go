package handler

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

func assertContractRanges(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`SELECT count(*) FROM requests r WHERE NOT EXISTS (SELECT 1 FROM request_ranges rr WHERE rr.request_id=r.id)`,
		`SELECT count(*) FROM request_ranges rr WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id=rr.request_id)`,
		`SELECT count(*) FROM requests r WHERE r.start_date<>(SELECT min(start_date) FROM request_ranges rr WHERE rr.request_id=r.id) OR r.end_date<>(SELECT max(end_date) FROM request_ranges rr WHERE rr.request_id=r.id)`,
		`SELECT count(*) FROM request_ranges a JOIN request_ranges b ON b.request_id=a.request_id AND b.start_date>a.start_date AND b.start_date<=date(a.end_date,'+1 day')`,
		`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('requests_default_range','requests_sync_range')`,
	} {
		var n int
		if err := d.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Fatalf("range invariant %s: %d %v", q, n, err)
		}
	}
}

func multiForm(hs *harness) url.Values {
	return url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12", "2026-10-15"}, "end": {"2026-10-13", "2026-10-16"}, "note": {"Two stretches"}}
}

// Repeated form entries must survive preview, submission, edit and every
// view; a handler that uses FormValue would silently fill the gap.
func TestMultiRangeApplyEditViewsAndCSV(t *testing.T) {
	hs := newHarness(t)
	defer assertContractRanges(t, hs.db)
	staff, head, hr := hs.signIn("staff1"), hs.signIn("headops"), hs.signIn("hr")
	v := multiForm(hs)
	_, body := hs.get(staff, "/leave/quote?"+v.Encode())
	if !strings.Contains(body, "4 working days in 2 stretches: Mon 12 – Tue 13 Oct, Thu 15 – Fri 16 Oct") {
		t.Fatalf("preview: %s", body)
	}
	res, body := hs.post(staff, "/leave", v)
	if res.StatusCode != 303 {
		t.Fatalf("submit: %d %s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/leave/"), 10, 64)
	rq, err := hs.svc.Get(t.Context(), id)
	if err != nil || len(rq.Ranges) != 2 || rq.Days != 400 {
		t.Fatalf("saved: %+v %v", rq, err)
	}
	for _, page := range []struct {
		c    *http.Client
		path string
	}{{staff, path}, {staff, "/"}, {head, "/approvals"}, {head, "/team"}, {hr, "/admin/requests"}} {
		code, body := hs.get(page.c, page.path)
		want := "Mon 12 – Tue 13 Oct and Thu 15 – Fri 16 Oct 2026"
		if page.path == "/team" {
			want = `<span>12–13 Oct</span><span>15–16 Oct</span>`
		}
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s", page.path, code, body)
		}
	}
	_, body = hs.get(staff, path+"/edit")
	if !strings.Contains(body, `value="2026-10-12/2026-10-13 2026-10-15/2026-10-16"`) || !strings.Contains(body, `multiple max-ranges="3"`) {
		t.Fatalf("edit widget: %s", body)
	}
	v["end"][1] = "2026-10-15"
	res, body = hs.post(staff, path+"/edit", v)
	if res.StatusCode != 303 {
		t.Fatalf("edit: %d %s", res.StatusCode, body)
	}
	rq, _ = hs.svc.Get(t.Context(), id)
	if len(rq.Ranges) != 2 || rq.End != "2026-10-15" || rq.Days != 300 {
		t.Fatalf("edited: %+v", rq)
	}
	_, body = hs.get(staff, "/team")
	if n := marked(gridRegion(t, body), "pending"); n != 3 {
		t.Errorf("own grid marks %d, want 3", n)
	}
	_, body = hs.get(head, "/team")
	if n := marked(gridRegion(t, body), "pending"); n != 3 {
		t.Errorf("colleague grid marks %d, want 3", n)
	}
	m, err := hs.h.pickerMarks(t.Context(), []leave.Request{rq})
	if err != nil || len(m.Booked) != 2 || m.Booked[1].Start != "2026-10-15" {
		t.Fatalf("marks: %+v %v", m, err)
	}
	res, err = hr.Get(hs.srv.URL + "/admin/requests.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rows, err := csv.NewReader(res.Body).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV: %v %v", rows, err)
	}
	cols := map[string]int{}
	for i, name := range rows[0] {
		cols[name] = i
	}
	if rows[1][cols["ranges"]] != "2026-10-12/2026-10-13 2026-10-15/2026-10-15" || rows[1][cols["start"]] != rq.Start || rows[1][cols["end"]] != rq.End {
		t.Fatalf("CSV: %v", rows)
	}
}

func TestMultiRangeRecordAndMismatchedPairs(t *testing.T) {
	hs := newHarness(t)
	defer assertContractRanges(t, hs.db)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	u, _ := hs.us.ByUsername(t.Context(), "staff1")
	record := fmt.Sprintf("/admin/users/%d/leave", u.ID)
	v := multiForm(hs)
	v["start"], v["end"] = []string{"2026-09-28", "2026-10-01"}, []string{"2026-09-29", "2026-10-02"}
	res, body := hs.post(hr, record, v)
	if res.StatusCode != 303 {
		t.Fatalf("record: %d %s", res.StatusCode, body)
	}
	rs, err := hs.svc.ForUser(t.Context(), u.ID)
	if err != nil || len(rs) != 1 || len(rs[0].Ranges) != 2 || rs[0].Days != 400 {
		t.Fatalf("recorded: %+v %v", rs, err)
	}
	path := hs.apply(staff, "annual", "2026-10-19", "2026-10-19")
	v = multiForm(hs)
	v["end"] = v["end"][:1]
	for _, target := range []struct {
		c    *http.Client
		path string
	}{{staff, "/leave"}, {staff, path + "/edit"}, {hr, record}} {
		res, body := hs.post(target.c, target.path, v)
		if res.StatusCode != 422 || !strings.Contains(body, "Choose the first and last day of each stretch.") {
			t.Errorf("mismatch %s: %d %s", target.path, res.StatusCode, body)
		}
	}
	_, body = hs.get(staff, "/leave/quote?"+v.Encode())
	if !strings.Contains(body, "Choose the first and last day of each stretch.") {
		t.Fatalf("preview mismatch: %s", body)
	}
}

func TestRangeErrorsBesideDatesWithHalfDaysDisabled(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	st, _ := hs.svc.Settings(t.Context())
	hr, _ := hs.us.ByUsername(t.Context(), "hr")
	day := time.Monday
	st.RestrictedDay = &day
	st.RestrictedMinDays = 3
	if err := hs.svc.SaveSettings(t.Context(), hr, st); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		starts, ends []string
		msg          string
	}{
		{[]string{"2026-10-06", "2026-10-08", "2026-10-13", "2026-10-15"}, []string{"2026-10-06", "2026-10-08", "2026-10-13", "2026-10-15"}, "at most 3 separate ranges"},
		{[]string{"2026-10-06", "2026-11-06"}, []string{"2026-10-06", "2026-11-06"}, "at most 31 days"},
		{[]string{"2026-10-12", "2026-10-15"}, []string{"2026-10-12", "2026-10-16"}, "is in a stretch of 1"},
	} {
		v := multiForm(hs)
		v["start"], v["end"] = tc.starts, tc.ends
		res, body := hs.post(staff, "/leave", v)
		beforeQuote, _, _ := strings.Cut(body, `<div id="quote"`)
		if res.StatusCode != 422 || !strings.Contains(beforeQuote, tc.msg) || strings.Contains(beforeQuote, `name="half_start"`) {
			t.Errorf("date refusal %q: %d %s", tc.msg, res.StatusCode, body)
		}
		_, body = hs.get(staff, "/leave/quote?"+v.Encode())
		if !strings.Contains(body, tc.msg) {
			t.Errorf("preview refusal %q: %s", tc.msg, body)
		}
	}
}

func TestHomeGapUsesNextStretch(t *testing.T) {
	r := leave.Request{ID: 1, Kind: leave.KindLeave, Status: leave.StatusApproved, Start: "2026-10-05", End: "2026-10-09", Ranges: []leave.Range{{Start: "2026-10-05", End: "2026-10-06"}, {Start: "2026-10-08", End: "2026-10-09"}}}
	for _, tc := range []struct {
		day        string
		now        bool
		start, end string
		in         int
	}{{"2026-10-05", true, "2026-10-05", "2026-10-06", 0}, {"2026-10-07", false, "2026-10-08", "2026-10-09", 1}, {"2026-10-08", true, "2026-10-08", "2026-10-09", 0}} {
		d, _ := leave.ParseDate(tc.day)
		n := newHomeData(nil, []leave.Request{r}, d).Next
		if n == nil || n.Now != tc.now || n.Start != tc.start || n.End != tc.end || n.In != tc.in {
			t.Errorf("%s: %+v", tc.day, n)
		}
	}
}

func TestMultiRangeAwayHintsExcludeGapOnlyColleagues(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	// The department head is away only on the gap day.
	hs.apply(head, "annual", "2026-10-14", "2026-10-14")
	v := multiForm(hs)
	_, body := hs.get(staff, "/leave/quote?"+v.Encode())
	if strings.Contains(body, "Olivia Operations") {
		t.Fatalf("gap-only colleague in preview: %s", body)
	}
	res, body := hs.post(staff, "/leave", v)
	if res.StatusCode != 303 {
		t.Fatalf("submit: %d %s", res.StatusCode, body)
	}
	_, body = hs.get(head, "/approvals")
	if strings.Contains(body, "Also away:") {
		t.Fatalf("gap-only colleague in queue: %s", body)
	}
	// A range that touches either requested stretch still appears.
	hs.apply(head, "annual", "2026-10-16", "2026-10-16")
	_, body = hs.get(staff, "/leave/quote?"+v.Encode())
	if !strings.Contains(body, "Olivia Operations") {
		t.Fatalf("intersecting colleague absent: %s", body)
	}
}

func TestMultiRangePermissionsAndSinglePairFallback(t *testing.T) {
	hs := newHarness(t)
	staff, head, hr := hs.signIn("staff1"), hs.signIn("headops"), hs.signIn("hr")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-30")
	// A single no-JS pair still restores in both the widget and native fields.
	_, body := hs.get(staff, path+"/edit")
	for _, want := range []string{`value="2026-10-12/2026-10-30"`, `id="start" name="start" value="2026-10-12"`, `id="end" name="end" value="2026-10-30"`} {
		if !strings.Contains(body, want) {
			t.Errorf("single fallback lacks %q", want)
		}
	}
	hs.approveAs(head, path)
	v := multiForm(hs)
	v["start"], v["end"] = []string{"2026-11-02", "2026-11-05"}, []string{"2026-11-03", "2026-11-06"}
	v.Set("negative", "1")
	res, body := hs.post(staff, "/leave", v)
	if res.StatusCode != 303 {
		t.Fatalf("ask HR: %d %s", res.StatusCode, body)
	}
	code, body := hs.get(hr, "/admin/permissions")
	if code != 200 || !strings.Contains(body, "Mon 2 – Tue 3 Nov and Thu 5 – Fri 6 Nov 2026") {
		t.Fatalf("permissions: %d %s", code, body)
	}
}

// Read the actual native controls: posting hand-written pairs would miss a
// template that accidentally expands the gap into leave when JavaScript is off.
func nativeDatePairs(t *testing.T, body string) url.Values {
	t.Helper()
	v := url.Values{}
	for _, m := range regexp.MustCompile(`<input type="date"[^>]*name="(start|end)" value="([^"]*)"[^>]*>`).FindAllStringSubmatch(body, -1) {
		v.Add(m[1], m[2])
	}
	if len(v["start"]) != 2 || len(v["end"]) != 2 {
		t.Fatalf("want two native date pairs, got %v", v)
	}
	return v
}

func TestMultiRangeNativeFormsPreserveGaps(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	v := multiForm(hs)
	res, body := hs.post(staff, "/leave", v)
	if res.StatusCode != 303 {
		t.Fatalf("apply: %d %s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	_, body = hs.get(staff, path+"/edit")
	native := nativeDatePairs(t, body)
	native.Set("type", v.Get("type"))
	native.Set("note", "Two stretches")
	res, body = hs.post(staff, path+"/edit", native)
	if res.StatusCode != 303 {
		t.Fatalf("native edit: %d %s", res.StatusCode, body)
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/leave/"), 10, 64)
	rq, err := hs.svc.Get(t.Context(), id)
	want := []leave.Range{{Start: "2026-10-12", End: "2026-10-13"}, {Start: "2026-10-15", End: "2026-10-16"}}
	if err != nil || !slices.Equal(rq.Ranges, want) || rq.Days != 400 {
		t.Fatalf("native edit changed ranges: %+v %v", rq, err)
	}

	u, _ := hs.us.ByUsername(t.Context(), "staff2")
	record := fmt.Sprintf("/admin/users/%d/leave", u.ID)
	v["start"], v["end"] = []string{"2026-09-28", "2026-10-01"}, []string{"2026-09-29", "2026-10-02"}
	want = []leave.Range{{Start: "2026-09-28", End: "2026-09-29"}, {Start: "2026-10-01", End: "2026-10-02"}}
	v.Set("note", "") // Required reason forces the record form to re-render.
	res, body = hs.post(hr, record, v)
	if res.StatusCode != 422 {
		t.Fatalf("record refusal: %d %s", res.StatusCode, body)
	}
	native = nativeDatePairs(t, body)
	native.Set("type", v.Get("type"))
	native.Set("note", "Recorded after checking dates")
	res, body = hs.post(hr, record, native)
	if res.StatusCode != 303 {
		t.Fatalf("native record: %d %s", res.StatusCode, body)
	}
	rs, err := hs.svc.ForUser(t.Context(), u.ID)
	if err != nil || len(rs) != 1 || !slices.Equal(rs[0].Ranges, want) || rs[0].Days != 400 {
		t.Fatalf("native record changed ranges: %+v %v", rs, err)
	}
}

func TestMultiRangeHomeWaitingToSubmit(t *testing.T) {
	hs := newHarness(t)
	staff, head, hr := hs.signIn("staff1"), hs.signIn("headops"), hs.signIn("hr")
	big := hs.apply(staff, "annual", "2026-10-12", "2026-10-30")
	hs.approveAs(head, big)
	v := multiForm(hs)
	v["start"], v["end"] = []string{"2026-11-02", "2026-11-05"}, []string{"2026-11-03", "2026-11-06"}
	v.Set("negative", "1")
	res, body := hs.post(staff, "/leave", v)
	if res.StatusCode != 303 {
		t.Fatalf("ask HR: %d %s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	res, body = hs.post(hr, "/admin/permissions/"+strings.TrimPrefix(path, "/leave/")+"/grant", url.Values{"note": {"ok"}})
	if res.StatusCode != 303 {
		t.Fatalf("grant: %d %s", res.StatusCode, body)
	}
	_, body = hs.get(staff, "/")
	_, waiting, ok := strings.Cut(body, "Waiting for you to submit</h2>")
	waiting, _, _ = strings.Cut(waiting, "</section>")
	if !ok || !strings.Contains(waiting, "Mon 2 – Tue 3 Nov and Thu 5 – Fri 6 Nov 2026") {
		t.Fatalf("waiting list hides gap: %s", waiting)
	}
}

func TestMultiRangeCancellationNotice(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	res, body := hs.post(staff, "/leave", multiForm(hs))
	if res.StatusCode != 303 {
		t.Fatalf("apply: %d %s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	hs.approveAs(head, path)
	res, body = hs.post(staff, path+"/cancel", url.Values{"note": {"plans changed"}})
	if res.StatusCode != 303 {
		t.Fatalf("cancel: %d %s", res.StatusCode, body)
	}
	_, body = hs.get(staff, res.Header.Get("Location"))
	_, notice, ok := strings.Cut(body, "This asks to cancel ")
	notice, _, _ = strings.Cut(notice, "</p>")
	if !ok || !strings.Contains(notice, "Mon 12 – Tue 13 Oct and Thu 15 – Fri 16 Oct 2026") {
		t.Fatalf("cancellation notice hides gap: %s", notice)
	}
}

func TestLeaveInputUsesBodyAndRejectsMalformedForms(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	res, body := hs.post(staff, "/leave?start=2026-10-14&end=2026-10-14&type=999&negative=1", multiForm(hs))
	if res.StatusCode != 303 {
		t.Fatalf("query altered POST: %d %s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	u, _ := hs.us.ByUsername(t.Context(), "staff2")
	for _, target := range []struct {
		client *http.Client
		path   string
	}{{staff, "/leave"}, {staff, path + "/edit"}, {hr, fmt.Sprintf("/admin/users/%d/leave", u.ID)}} {
		req, _ := http.NewRequest(http.MethodPost, hs.srv.URL+target.path, strings.NewReader(multiForm(hs).Encode()+"&note=%ZZ"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := target.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("malformed %s: %d", target.path, res.StatusCode)
		}
	}
	// MaxBytesReader's typed error is mapped to 413, rather than 500 or a
	// partially valid request after a failed read.
	r := httptest.NewRequest(http.MethodPost, "/leave", strings.NewReader("start="+strings.Repeat("x", 32)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, 10)
	_, err := inputFrom(r)
	if !unreadableInput(w, err) || w.Code != 413 {
		t.Fatalf("oversized form: %d %v", w.Code, err)
	}
}
