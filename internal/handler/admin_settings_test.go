package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func validSettingsPost(over url.Values) url.Values {
	v := url.Values{"day": {"mon", "tue", "wed", "thu", "fri"}, "restricted_day": {""}, "restricted_min": {"3"},
		"final_approver": {""}, "sick_cycle": {"2025"}}
	for k, val := range over {
		v[k] = val
	}
	return v
}

// Admin input is parsed strictly: a value no form can produce is a 400, and a
// value a person can mistype comes back as an error beside its field with
// what they typed, never a quiet default.
func TestAdminSettingsValidation(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")

	for name, over := range map[string]url.Values{
		"unknown day":            {"day": {"mon", "funday"}},
		"unknown restricted day": {"restricted_day": {"monday"}},
		"final approver text":    {"final_approver": {"pastor"}},
	} {
		if res, _ := hs.post(hr, "/admin/settings", validSettingsPost(over)); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, res.StatusCode)
		}
	}

	for name, c := range map[string]struct {
		over url.Values
		want string
	}{
		"no days":                   {url.Values{"day": nil}, "Choose at least one working day."},
		"restricted day not worked": {url.Values{"restricted_day": {"sun"}}, "must be one of the working days"},
		"min empty":                 {url.Values{"restricted_min": {""}}, "whole number from 1 to 31"},
		"min signed":                {url.Values{"restricted_min": {"+5"}}, "whole number from 1 to 31"},
		"min too big":               {url.Values{"restricted_min": {"32"}}, "whole number from 1 to 31"},
		"cycle small":               {url.Values{"sick_cycle": {"1999"}}, "2000 to 2100"},
		"approver missing":          {url.Values{"final_approver": {"9999"}}, "currently enabled"},
	} {
		res, body := hs.post(hr, "/admin/settings", validSettingsPost(c.over))
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d, want 422 mentioning %q", name, res.StatusCode, c.want)
		}
	}

	// What was typed comes back, so one bad field does not cost the rest.
	_, body := hs.post(hr, "/admin/settings", validSettingsPost(url.Values{"sick_cycle": {"two weeks"}, "restricted_min": {"5"}}))
	if !strings.Contains(body, `value="two weeks"`) || !strings.Contains(body, `value="5"`) {
		t.Error("a rejected form did not show what was entered")
	}
}

// A saved change reaches the next request with no restart, and the page says
// who made it.
func TestAdminSettingsSaveApplies(t *testing.T) {
	hs := newHarness(t)
	hr, staff := hs.signIn("hr"), hs.signIn("staff1")
	pastor, _ := hs.us.ByUsername(t.Context(), "pastor")

	monday := url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"}, "end": {"2026-10-12"}, "note": {"n"}}
	if code, body := hs.get(staff, "/leave/quote?"+monday.Encode()); code != 200 || strings.Contains(body, "days off or public holidays") {
		t.Fatalf("Monday before: %d", code)
	}

	res, _ := hs.post(hr, "/admin/settings", validSettingsPost(url.Values{"day": {"tue", "wed", "thu", "fri"},
		"final_approver": {itoa(pastor.ID)}}))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/admin/settings?notice=saved" {
		t.Fatalf("save: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if code, body := hs.get(staff, "/leave/quote?"+monday.Encode()); code != 200 || !strings.Contains(body, "days off or public holidays") {
		t.Fatalf("Monday after the week changed: %d", code)
	}

	code, body := hs.get(hr, "/admin/settings?notice=saved")
	if code != 200 || !strings.Contains(body, "Last changed by Hannah HR") || !strings.Contains(body, "Saved.") {
		t.Fatalf("settings page: %d", code)
	}
	if !strings.Contains(body, `value="tue" checked`) || strings.Contains(body, `value="mon" checked`) {
		t.Error("the page does not show the saved working days")
	}
}
