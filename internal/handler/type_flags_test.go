package handler

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestAdminTypeFlagsRejectStaleOrInvalidForms(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	id := hs.typeID("annual")
	before, err := hs.svc.Type(t.Context(), mustAtoi(t, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"staff_can_apply", "show_balance"} {
		for _, value := range []string{"missing", "", "garbage", "2", " 1"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				form := url.Values{"id": {id}, "name": {"Must not save"}, "cycle_months": {"12"},
					"unit": {"workdays"}, "restricted": {"any"}, "coverage_rule": {"count"},
					"backdate_days": {"92"}, "active": {"1"},
					"staff_can_apply": {"0", "1"}, "show_balance": {"0", "1"}}
				if value == "missing" {
					form.Del(field)
				} else {
					form[field] = []string{"0", value}
				}
				res, body := hs.post(hr, "/admin/types", form)
				if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Reload the form") {
					t.Fatalf("invalid %s: %d %s", field, res.StatusCode, body)
				}
				if !strings.Contains(body, `value="Must not save"`) {
					t.Error("form input not retained")
				}
				for _, other := range []string{"staff_can_apply", "show_balance"} {
					if strings.Contains(body, `name="`+other+`" value="1" checked`) != (other != field) {
						t.Errorf("wrong re-rendered checkbox state for %s", other)
					}
				}
				after, err := hs.svc.Type(t.Context(), before.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("rejected form changed type: before=%+v after=%+v err=%v", before, after, err)
				}
			})
		}
	}
}

func TestAdminTypeFlagsRetainedOnValidationError(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	for _, value := range []string{"0", "1"} {
		form := url.Values{"name": {""}, "staff_can_apply": {"0", value}, "show_balance": {"0", value}}
		res, body := hs.post(hr, "/admin/types", form)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("invalid form: %d", res.StatusCode)
		}
		for _, field := range []string{"staff_can_apply", "show_balance"} {
			if strings.Contains(body, `name="`+field+`" value="1" checked`) != (value == "1") {
				t.Errorf("%s lost value %s on error", field, value)
			}
			hidden := `<input type="hidden" name="` + field + `" value="0">`
			checkbox := `<input type="checkbox" name="` + field + `" value="1"`
			if h, c := strings.Index(body, hidden), strings.Index(body, checkbox); h < 0 || c < h {
				t.Errorf("%s needs hidden zero before checkbox", field)
			}
		}
	}
}
