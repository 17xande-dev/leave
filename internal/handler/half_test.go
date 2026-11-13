package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func setHalfDays(t *testing.T, hs *harness, enabled bool) {
	t.Helper()
	st, err := hs.svc.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hr, err := hs.us.ByUsername(t.Context(), "hr")
	if err != nil {
		t.Fatal(err)
	}
	st.HalfDaysEnabled = enabled
	if err := hs.svc.SaveSettings(t.Context(), hr, st); err != nil {
		t.Fatal(err)
	}
}

// The apply form carries both checkboxes, a posted box makes the half day, and
// the request page, the preview and the lists all say which day is half.
func TestHalfDayThroughTheApplyForm(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, true)
	staff := hs.signIn("staff1")

	_, body := hs.get(staff, "/leave/new")
	for _, want := range []string{`name="half_start" value="1"`, `name="half_end" value="1"`,
		"First (or only) day is a half day", "Last day is a half day"} {
		if !strings.Contains(body, want) {
			t.Errorf("apply page lacks %q", want)
		}
	}

	// Mon 12 - Tue 13 Oct with the last day a half day: 1.5 days.
	res, body := hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"},
		"end": {"2026-10-13"}, "note": {"n"}, "half_end": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply: %d\n%s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	_, body = hs.get(staff, path)
	if !strings.Contains(body, "1.5 working days") || !strings.Contains(body, "last day a half day") {
		t.Fatalf("request page:\n%s", body)
	}
	_, body = hs.get(staff, "/")
	if !strings.Contains(body, "last day a half day") || !strings.Contains(body, "1.5") {
		t.Fatalf("home page does not say which day is half:\n%s", body)
	}
	// The approver's queue shows it too.
	_, body = hs.get(hs.signIn("headops"), "/approvals")
	if !strings.Contains(body, "1.5") || !strings.Contains(body, "last day a half day") {
		t.Fatalf("approvals:\n%s", body)
	}
}

func TestHalfDayOnASingleDay(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, true)
	staff := hs.signIn("staff1")
	res, body := hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-14"},
		"end": {"2026-10-14"}, "note": {"n"}, "half_start": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply: %d\n%s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	_, body = hs.get(staff, path)
	if !strings.Contains(body, "0.5 working days") || !strings.Contains(body, "(half day)") {
		t.Fatalf("request page:\n%s", body)
	}
}

// A refused flag re-renders the form with the message beside the boxes and the
// boxes still ticked, so nothing has to be re-entered.
func TestHalfDayRefusalIsShownInline(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, true)
	staff := hs.signIn("staff1")
	res, body := hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"},
		"end": {"2026-10-12"}, "note": {"n"}, "half_end": {"1"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `class="error"`) ||
		!strings.Contains(body, "A one-day request has only a first day") {
		t.Fatalf("half_end on one day: %d\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `name="half_end" value="1" checked`) {
		t.Error("the ticked box was lost on re-render")
	}
}

// The live preview reads the boxes from the form's query.
func TestQuotePartialCountsHalfDays(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, true)
	c := hs.signIn("staff1")
	_, body := hs.get(c, fmt.Sprintf("/leave/quote?type=%s&start=2026-10-12&end=2026-10-14&half_end=1", hs.typeID("annual")))
	if !strings.Contains(body, "2.5 working days") || !strings.Contains(body, "12.5 left") {
		t.Fatalf("quote:\n%s", body)
	}
	_, body = hs.get(c, fmt.Sprintf("/leave/quote?type=%s&start=2026-10-17&end=2026-10-19&half_start=1", hs.typeID("annual")))
	if !strings.Contains(body, "cannot be a half day") {
		t.Fatalf("quote of a half day on a Saturday:\n%s", body)
	}
}

// Editing starts from the request's own flags, and saving can change them.
func TestEditFormKeepsAndChangesHalfDays(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, true)
	staff := hs.signIn("staff1")
	res, _ := hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"},
		"end": {"2026-10-13"}, "note": {"n"}, "half_end": {"1"}})
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	_, body := hs.get(staff, path+"/edit")
	if !strings.Contains(body, `name="half_end" value="1" checked`) || strings.Contains(body, `name="half_start" value="1" checked`) {
		t.Fatalf("edit form did not carry the flags:\n%s", body)
	}
	res, body = hs.post(staff, path+"/edit", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"},
		"end": {"2026-10-13"}, "note": {"n"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit: %d\n%s", res.StatusCode, body)
	}
	_, body = hs.get(staff, path)
	if !strings.Contains(body, "2 working days") || strings.Contains(body, "half day") {
		t.Fatalf("request after removing the flag:\n%s", body)
	}
}

// The admin's record form takes the same boxes.
func TestAdminRecordLeaveWithAHalfDay(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	page := "/admin/users/" + itoa(hs.staffID("staff1"))
	_, body := hs.get(hr, page)
	if strings.Count(body, `name="half_start" value="1"`) != 1 || strings.Count(body, `name="half_end" value="1"`) != 1 {
		t.Fatalf("record form lacks the half-day boxes:\n%s", body)
	}
	res, body := hs.post(hr, page+"/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-06-22"},
		"end": {"2026-06-23"}, "note": {"from the log"}, "half_end": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("record: %d\n%s", res.StatusCode, body)
	}
	_, body = hs.get(hr, page)
	if !strings.Contains(body, "1.5") || !strings.Contains(body, "last day a half day") {
		t.Fatalf("page after recording:\n%s", body)
	}
	res, body = hs.post(hr, page+"/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-06-24"},
		"end": {"2026-06-24"}, "note": {"x"}, "half_end": {"1"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "A one-day request has only a first day") {
		t.Fatalf("refused record: %d\n%s", res.StatusCode, body)
	}
}

// A stale form can still post half-day flags after an admin disables them.
// Both forms must explain the refusal even though the controls are hidden.
func TestHalfDaysOffRefusalIsShownOnApplyAndEdit(t *testing.T) {
	hs := newHarness(t)
	setHalfDays(t, hs, false)
	staff := hs.signIn("staff1")
	form := url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"}, "end": {"2026-10-13"}}
	res, body := hs.post(staff, "/leave", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("whole-day apply: %d\n%s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	for _, endpoint := range []string{"/leave", path + "/edit"} {
		for _, field := range []string{"half_start", "half_end"} {
			t.Run(endpoint+"/"+field, func(t *testing.T) {
				form.Set(field, "1")
				defer form.Del(field)
				res, body := hs.post(staff, endpoint, form)
				if res.StatusCode != http.StatusUnprocessableEntity ||
					!strings.Contains(body, "Half days are not available. Book whole days.") {
					t.Fatalf("refused half day: %d\n%s", res.StatusCode, body)
				}
			})
		}
	}
}

func TestHalfDaysSettingControlsFormsAndEdit(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	boxes := func(path string, present bool, client *http.Client) string {
		t.Helper()
		code, body := hs.get(client, path)
		if code != http.StatusOK {
			t.Fatalf("%s: %d\n%s", path, code, body)
		}
		for _, name := range []string{"half_start", "half_end"} {
			if strings.Contains(body, `name="`+name+`"`) != present {
				t.Errorf("%s: %s presence, want %v", path, name, present)
			}
		}
		if !present && strings.Contains(body, `class="checks"`) {
			t.Errorf("%s: hidden half-day checks block remains", path)
		}
		return body
	}
	boxes("/leave/new", false, staff)
	page := "/admin/users/" + itoa(hs.staffID("staff1"))
	boxes(page, true, hr)
	setHalfDays(t, hs, true)
	boxes("/leave/new", true, staff)
	form := url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"}, "end": {"2026-10-13"}, "half_end": {"1"}}
	res, body := hs.post(staff, "/leave", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply: %d\n%s", res.StatusCode, body)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	boxes(path+"/edit", true, staff)
	setHalfDays(t, hs, false)
	body = boxes(path+"/edit", false, staff)
	if !strings.Contains(body, "2 working days") {
		t.Fatalf("edit preview must show whole days before saving:\n%s", body)
	}
	boxes(page, true, hr)
	form.Del("half_end")
	res, body = hs.post(staff, path+"/edit", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("whole-day edit: %d\n%s", res.StatusCode, body)
	}
	_, body = hs.get(staff, path)
	if !strings.Contains(body, "2 working days") || strings.Contains(body, "last day a half day") {
		t.Fatalf("saved edit must be whole days:\n%s", body)
	}
}
