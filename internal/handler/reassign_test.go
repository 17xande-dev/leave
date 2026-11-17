package handler

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/notify"
)

func approverSelect(t *testing.T, body string) string {
	t.Helper()
	s := regexp.MustCompile(`(?s)<select id="approver".*?</select>`).FindString(body)
	if s == "" {
		t.Fatal("no approver select")
	}
	return s
}

func TestReassignFormTargets(t *testing.T) {
	hs := newHarness(t)
	if _, err := hs.db.Exec(`UPDATE users SET is_admin = 1 WHERE username = 'headkids'`); err != nil {
		t.Fatal(err)
	}
	hr := hs.signIn("hr")
	own := hs.apply(hr, "annual", "2026-10-12", "2026-10-12")
	other := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	for _, path := range []string{own, other} {
		_, body := hs.get(hr, path)
		s := approverSelect(t, body)
		if !strings.Contains(body, "Currently with Olivia Operations") ||
			!strings.Contains(s, `name="approver" required`) ||
			!strings.Contains(s, `<option value="" selected disabled>Choose…</option>`) ||
			strings.Count(s, "selected") != 1 ||
			!strings.Contains(s, `<option value="0">Leave administrators</option>`) ||
			!strings.Contains(s, "Kagiso Kids") {
			t.Fatalf("unsafe or incomplete form: %s", s)
		}
		for _, name := range []string{"Sam Staff", "Thandi Teacher", "Olivia Operations", "Senior Pastor"} {
			if strings.Contains(s, name) {
				t.Fatalf("non-admin/applicant option %s: %s", name, s)
			}
		}
		if path == own && strings.Contains(s, "Hannah HR") {
			t.Fatal("own applicant is an option")
		}
	}
	action := "/admin/requests" + strings.TrimPrefix(own, "/leave") + "/reassign"
	for _, tc := range []struct {
		value, notice string
		code          int
	}{
		{itoa(hs.staffID("hr")), "reassign-owner", 303},
		{itoa(hs.staffID("staff2")), "reassign-not-admin", 303},
		{"", "", 400},
		{itoa(hs.staffID("headkids")), "reassigned", 303},
	} {
		res, _ := hs.post(hr, action, url.Values{"approver": {tc.value}})
		if res.StatusCode != tc.code || tc.notice != "" && !strings.HasSuffix(res.Header.Get("Location"), "?notice="+tc.notice) {
			t.Fatalf("value %q: %d %s", tc.value, res.StatusCode, res.Header.Get("Location"))
		}
		rs, err := hs.svc.ForUser(t.Context(), hs.staffID("hr"))
		if err != nil || len(rs) != 1 {
			t.Fatalf("requests %v %v", rs, err)
		}
		want := hs.staffID("headops")
		if tc.notice == "reassigned" {
			want = hs.staffID("headkids")
		}
		if rs[0].ApproverID != want {
			t.Fatalf("approver=%d want=%d", rs[0].ApproverID, want)
		}
	}
	// Even when the current approver is eligible, reassignment must be explicit.
	_, body := hs.get(hr, own)
	s := approverSelect(t, body)
	if !strings.Contains(s, `<option value="`+itoa(hs.staffID("headkids"))+`">Kagiso Kids</option>`) ||
		strings.Count(s, "selected") != 1 ||
		!strings.Contains(s, `<option value="" selected disabled>Choose…</option>`) {
		t.Fatalf("current administrator was preselected: %s", s)
	}
	// With another admin the group queue is still a useful choice.
	res, _ := hs.post(hr, action, url.Values{"approver": {"0"}})
	if res.StatusCode != 303 || !strings.HasSuffix(res.Header.Get("Location"), "?notice=reassigned") {
		t.Fatalf("queue with second admin: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// No other admin remains: the owner cannot decide their own queue entry.
	if _, err := hs.db.Exec(`UPDATE users SET is_admin = 0 WHERE username = 'headkids'`); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(hr, own)
	if s := approverSelect(t, body); strings.Contains(s, `<option value="0">`) ||
		!strings.Contains(body, "No other enabled administrator exists. Make someone else an administrator") {
		t.Fatal(body)
	}
	res, _ = hs.post(hr, action, url.Values{"approver": {"0"}})
	if res.StatusCode != 303 || !strings.HasSuffix(res.Header.Get("Location"), "?notice=reassign-no-other-admin") {
		t.Fatalf("queue with only owner: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body = hs.get(hr, res.Header.Get("Location"))
	if !strings.Contains(body, leave.MsgReassignNoOtherAdmin) {
		t.Fatal(body)
	}
	// The lone administrator can still decide someone else's queued request.
	_, body = hs.get(hr, other)
	if !strings.Contains(approverSelect(t, body), `<option value="0">Leave administrators</option>`) {
		t.Fatal(body)
	}
}

func TestOverrideFormTargets(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	path := "/admin/users/" + itoa(hs.staffID("hr"))
	_, body := hs.get(hr, path)
	s := approverSelect(t, body)
	if strings.Contains(s, "Hannah HR") || strings.Contains(s, "Sam Staff") || !strings.Contains(s, "Directory default") {
		t.Fatal(s)
	}
	for _, target := range []string{"staff1", "hr"} {
		res, body := hs.post(hr, path+"/approver", url.Values{"approver": {itoa(hs.staffID(target))}})
		want := leave.MsgOverrideNotAdmin
		if target == "hr" {
			want = "Nobody can approve their own leave."
		}
		if res.StatusCode != 422 || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %s", target, res.StatusCode, body)
		}
	}
	if _, err := hs.db.Exec(`INSERT INTO approver_overrides VALUES (?, ?)`, hs.staffID("hr"), hs.staffID("staff1")); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(hr, path)
	s = approverSelect(t, body)
	if !strings.Contains(body, "Current override: Sam Staff — not an enabled administrator; kept until changed.") ||
		!strings.Contains(s, `<option value="" selected disabled>Choose…</option>`) || strings.Contains(s, "Sam Staff") {
		t.Fatal(body)
	}
	if res, _ := hs.post(hr, path+"/approver", url.Values{"approver": {"0"}}); res.StatusCode != 303 {
		t.Fatalf("clear=%d", res.StatusCode)
	}
	// Disabled overrides are likewise shown as legacy text, not options.
	if _, err := hs.db.Exec(`UPDATE users SET is_admin = 1, disabled = 1 WHERE username = 'staff2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.db.Exec(`INSERT INTO approver_overrides VALUES (?, ?)`, hs.staffID("hr"), hs.staffID("staff2")); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(hr, path)
	if s := approverSelect(t, body); strings.Contains(s, "Thandi Teacher") ||
		!strings.Contains(body, "Current override: Thandi Teacher — not an enabled administrator; kept until changed.") {
		t.Fatal(body)
	}
}

func TestMissingReassignmentSubjects(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	res, _ := hs.post(hr, "/admin/requests/9999/reassign", url.Values{"approver": {"0"}})
	if res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(res.Header.Get("Location"), "?notice=not-possible") {
		t.Fatalf("missing request: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	for _, target := range []string{"0", itoa(hs.staffID("hr"))} {
		res, _ := hs.post(hr, "/admin/users/9999/approver", url.Values{"approver": {target}})
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("missing person, approver %s: %d", target, res.StatusCode)
		}
	}
}

func TestReassignNotification(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	hs.svc.OnEvent(notify.New("https://leave.test").Hook)
	action := "/admin/requests" + strings.TrimPrefix(path, "/leave") + "/reassign"
	hr := hs.signIn("hr")
	for _, target := range []string{"staff2", "staff1", "hr"} {
		res, _ := hs.post(hr, action, url.Values{"approver": {itoa(hs.staffID(target))}})
		if res.StatusCode != http.StatusSeeOther {
			t.Fatal(res.StatusCode)
		}
		var count int
		if err := hs.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if target != "hr" && count != 0 {
			t.Fatal("refusal enqueued mail")
		}
		if target == "hr" {
			var payload string
			if count != 1 {
				t.Fatalf("jobs=%d", count)
			}
			if err := hs.db.QueryRow(`SELECT payload FROM outbox`).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(payload, "hr@example.test") || !strings.Contains(payload, "reassigned to you") {
				t.Fatal(payload)
			}
		}
	}
}
