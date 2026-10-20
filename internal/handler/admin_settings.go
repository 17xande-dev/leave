package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// --- Settings ------------------------------------------------------------

// settingsForm is the form as typed, as text, so a rejected submission comes
// back showing exactly what was entered rather than a normalised version.
type settingsForm struct {
	Days          map[string]bool
	RestrictedDay string
	RestrictedMin string
	FinalApprover int64
	SickBackdate  string
	SickCycle     string
	TeamDefault   string
}

type settingsData struct {
	Form       settingsForm
	Errors     leave.FieldErrors
	Days       []leave.Day
	Candidates []users.User
	// UpdatedBy and UpdatedAt describe the last change; empty on a database
	// nobody has saved settings on yet.
	UpdatedBy string
	UpdatedAt string
}

func formFrom(st leave.Settings) settingsForm {
	f := settingsForm{Days: map[string]bool{}, RestrictedMin: strconv.Itoa(st.RestrictedMinDays),
		FinalApprover: st.FinalApproverID, SickBackdate: strconv.Itoa(st.SickBackdateDays),
		SickCycle: strconv.Itoa(st.SickCycleAnchor)}
	if st.TeamDefaultPercent > 0 {
		f.TeamDefault = strconv.Itoa(st.TeamDefaultPercent)
	}
	for _, d := range leave.Days {
		f.Days[d.Code] = st.Week.Has(d.Weekday)
		if st.RestrictedDay != nil && *st.RestrictedDay == d.Weekday {
			f.RestrictedDay = d.Code
		}
	}
	return f
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request, status int, form *settingsForm, errs leave.FieldErrors) {
	st, err := h.leave.Settings(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d := settingsData{Errors: errs, Days: leave.Days, UpdatedBy: st.UpdatedBy}
	if form != nil {
		d.Form = *form
	} else {
		d.Form = formFrom(st)
	}
	if t, err := time.Parse(time.RFC3339, st.UpdatedAt); err == nil {
		d.UpdatedAt = t.UTC().Format("2 Jan 2006 15:04")
	}
	enabled, err := h.users.Enabled(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d.Candidates = enabled
	// A final approver who has since been disabled stays listed (and marked),
	// so the page does not silently show "None" while the row says otherwise.
	// Saving it unchanged is then refused, which is the honest outcome.
	if id := d.Form.FinalApprover; id != 0 {
		listed := false
		for _, u := range enabled {
			listed = listed || u.ID == id
		}
		if u, err := h.users.ByID(r.Context(), id); !listed && err == nil {
			d.Candidates = append(d.Candidates, u)
		}
	}
	h.render(w, r, status, "admin_settings.html", view{Title: "Settings", Data: d})
}

func (h *Handler) AdminSettings(w http.ResponseWriter, r *http.Request) {
	h.settingsPage(w, r, http.StatusOK, nil, nil)
}

// parseWhole reads a whole number strictly: digits only, so "+5", "-0", " 7 "
// with inner spaces and "1e3" are all refused, and an empty field is an error
// rather than a quiet default.
func parseWhole(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 6 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func (h *Handler) AdminSaveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	form := settingsForm{Days: map[string]bool{}, RestrictedDay: f.Get("restricted_day"),
		RestrictedMin: strings.TrimSpace(f.Get("restricted_min")), SickBackdate: strings.TrimSpace(f.Get("sick_backdate")),
		SickCycle: strings.TrimSpace(f.Get("sick_cycle")), TeamDefault: strings.TrimSpace(f.Get("team_default"))}
	var in leave.Settings

	// A day code outside the seven is a hand-made post, not a typo to
	// explain next to a checkbox.
	var days []time.Weekday
	for _, code := range f["day"] {
		d, ok := leave.ParseDay(code)
		if !ok {
			http.Error(w, "Unknown day.", http.StatusBadRequest)
			return
		}
		form.Days[code] = true
		days = append(days, d)
	}
	in.Week = leave.WeekOf(days...)

	if form.RestrictedDay != "" {
		d, ok := leave.ParseDay(form.RestrictedDay)
		if !ok {
			http.Error(w, "Unknown day.", http.StatusBadRequest)
			return
		}
		in.RestrictedDay = &d
	}
	if v := f.Get("final_approver"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "Unknown person.", http.StatusBadRequest)
			return
		}
		in.FinalApproverID, form.FinalApprover = id, id
	}

	errs := leave.FieldErrors{}
	num := func(raw string, dst *int, key, msg string) {
		n, ok := parseWhole(raw)
		if !ok {
			errs[key] = msg
			return
		}
		*dst = n
	}
	num(form.RestrictedMin, &in.RestrictedMinDays, "restricted_min", "Enter a whole number from 1 to 31.")
	num(form.SickBackdate, &in.SickBackdateDays, "sick_backdate", "Enter a whole number of days from 0 to 365.")
	num(form.SickCycle, &in.SickCycleAnchor, "sick_cycle", "Enter a year from 2000 to 2100.")

	// Empty is no default, as on the Teams page.
	if n, ok := parsePercent(form.TeamDefault); ok {
		in.TeamDefaultPercent = n
	} else {
		errs["team_default"] = "Enter a whole number from 1 to 100, or leave it empty for no default."
	}

	if len(errs) == 0 {
		err := h.leave.SaveSettings(r.Context(), currentUser(r), in)
		var fe leave.FieldErrors
		switch {
		case err == nil:
			http.Redirect(w, r, "/admin/settings?notice=saved", http.StatusSeeOther)
			return
		case errors.As(err, &fe):
			errs = fe
		default:
			h.serverError(w, r, err)
			return
		}
	}
	h.settingsPage(w, r, http.StatusUnprocessableEntity, &form, errs)
}
