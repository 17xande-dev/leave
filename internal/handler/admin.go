package handler

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

func (h *Handler) adminRoutes() []route {
	a := func(pattern string, f http.HandlerFunc) route { return route{pattern, h.admin(f)} }
	return []route{
		a("GET /admin", h.AdminHome),
		a("GET /admin/requests", h.AdminRequests),
		a("GET /admin/requests.csv", h.AdminRequestsCSV),
		a("POST /admin/requests/{id}/reassign", h.AdminReassign),
		a("GET /admin/users", h.AdminUsers),
		a("GET /admin/users/{id}", h.AdminUser),
		a("POST /admin/users/{id}/approver", h.AdminSetApprover),
		a("POST /admin/users/{id}/adjustments", h.AdminAddAdjustment),
		a("POST /admin/users/{id}/long-weekend", h.AdminSetLongWeekend),
		a("POST /admin/users/{id}/leave", h.AdminRecordLeave),
		a("POST /admin/requests/{id}/unrecord", h.AdminUnrecord),
		a("GET /admin/types", h.AdminTypes),
		a("GET /admin/types/new", h.AdminTypeForm),
		a("GET /admin/types/{id}", h.AdminTypeForm),
		a("POST /admin/types", h.AdminSaveType),
		a("GET /admin/holidays", h.AdminHolidays),
		a("POST /admin/holidays", h.AdminAddHoliday),
		a("POST /admin/holidays/generate", h.AdminGenerateHolidays),
		a("POST /admin/holidays/delete", h.AdminDeleteHoliday),
		a("GET /admin/shutdowns", h.AdminShutdowns),
		a("GET /admin/shutdowns/new", h.AdminShutdownForm),
		a("POST /admin/shutdowns", h.AdminCreateShutdown),
		a("GET /admin/shutdowns/{id}", h.AdminShutdown),
		a("POST /admin/shutdowns/{id}/generate", h.AdminGenerateDrafts),
		a("POST /admin/shutdowns/{id}/delete", h.AdminDeleteShutdown),
		a("GET /admin/permissions", h.AdminPermissions),
		a("POST /admin/permissions/{id}/grant", h.AdminGrantNegative),
		a("POST /admin/permissions/{id}/refuse", h.AdminRefuseNegative),
		a("GET /admin/coverage", h.AdminCoverage),
		a("POST /admin/coverage", h.AdminSaveCoverage),
		a("GET /admin/teams", h.AdminTeams),
		a("POST /admin/teams", h.AdminSaveTeam),
		a("POST /admin/teams/default", h.AdminSaveTeamDefault),
		a("GET /admin/settings", h.AdminSettings),
		a("POST /admin/settings", h.AdminSaveSettings),
		a("GET /admin/outbox", h.AdminOutbox),
		a("POST /admin/outbox/{id}/retry", h.AdminRetry),
	}
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

type adminHomeData struct {
	Pending    int
	AwaitingHR int
	Problems   int
	Users      int
}

func (h *Handler) AdminHome(w http.ResponseWriter, r *http.Request) {
	pending, err := h.leave.All(r.Context(), leave.Filter{Status: leave.StatusPending})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	problems, err := h.outbox.Problems(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	us, err := h.users.Enabled(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	hr, err := h.leave.AwaitingHR(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_home.html", view{Title: "Admin",
		Data: adminHomeData{Pending: len(pending), AwaitingHR: len(hr), Problems: len(problems), Users: len(us)}})
}

// --- Requests ------------------------------------------------------------

type adminRequestsData struct {
	Filter   leave.Filter
	Requests []leave.Request
	Users    []users.User
	Query    string
}

var statuses = map[string]bool{"": true, leave.StatusPending: true, leave.StatusApproved: true,
	leave.StatusDeclined: true, leave.StatusWithdrawn: true, leave.StatusCancelled: true,
	leave.StatusAwaitingHR: true, leave.StatusDraft: true}

func filterFrom(r *http.Request) leave.Filter {
	q := r.URL.Query()
	f := leave.Filter{Status: q.Get("status"), From: q.Get("from"), To: q.Get("to")}
	if !statuses[f.Status] {
		f.Status = ""
	}
	// Dates go into a string comparison, so anything that is not a date is
	// dropped rather than compared.
	if _, err := leave.ParseDate(f.From); err != nil {
		f.From = ""
	}
	if _, err := leave.ParseDate(f.To); err != nil {
		f.To = ""
	}
	f.UserID, _ = strconv.ParseInt(q.Get("user"), 10, 64)
	return f
}

func (h *Handler) AdminRequests(w http.ResponseWriter, r *http.Request) {
	f := filterFrom(r)
	rs, err := h.leave.All(r.Context(), f)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	us, err := h.users.All(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_requests.html", view{Title: "All requests",
		Data: adminRequestsData{Filter: f, Requests: rs, Users: us, Query: r.URL.RawQuery}})
}

// AdminRequestsCSV exports the filtered list. It is built in a buffer first:
// streaming would commit a 200 before a failure, handing over a truncated
// file that looks complete.
func (h *Handler) AdminRequestsCSV(w http.ResponseWriter, r *http.Request) {
	rs, err := h.leave.All(r.Context(), filterFrom(r))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	cw.Write([]string{"id", "name", "department", "type", "kind", "start", "end", "days", "status",
		"approver", "decided_by", "decided_at", "note", "decision_note", "submitted_at", "recorded_by"})
	for _, q := range rs {
		cw.Write([]string{strconv.FormatInt(q.ID, 10), csvText(q.UserName), csvText(q.Department),
			csvText(q.TypeName), q.Kind, q.Start, q.End, strconv.Itoa(q.Days), q.Status,
			csvText(q.ApproverName), csvText(q.DecidedBy), q.DecidedAt, csvText(q.Note),
			csvText(q.DecisionNote), q.CreatedAt, csvText(q.RecordedBy)})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="leave-%s.csv"`, time.Now().Format("2006-01-02")))
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// csvText neutralises a cell a spreadsheet would evaluate. Excel and Sheets
// treat a leading =, +, -, @ (and tab/CR) as a formula, so a leave note
// reading "=HYPERLINK(...)" would become live on an admin's machine.
func csvText(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (h *Handler) AdminReassign(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	approver, err := strconv.ParseInt(r.PostFormValue("approver"), 10, 64)
	if err != nil {
		http.Error(w, "Choose an approver.", http.StatusBadRequest)
		return
	}
	_, err = h.leave.Reassign(r.Context(), currentUser(r), id, approver)
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe), errors.Is(err, leave.ErrNotAllowed):
		notPossible(w, r, id)
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=reassigned", id), http.StatusSeeOther)
}

// --- Users ---------------------------------------------------------------

type userRow struct {
	User     users.User
	Approver string
	Override bool
}

func (h *Handler) approverName(r *http.Request, u users.User) (string, error) {
	id, err := h.leave.ResolveApprover(r.Context(), u)
	if err != nil || id == 0 {
		return "Leave administrators", err
	}
	a, err := h.users.ByID(r.Context(), id)
	if err != nil {
		return "", err
	}
	return a.Name, nil
}

func (h *Handler) AdminUsers(w http.ResponseWriter, r *http.Request) {
	us, err := h.users.All(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows := make([]userRow, 0, len(us))
	for _, u := range us {
		row := userRow{User: u}
		if !u.Disabled {
			if row.Approver, err = h.approverName(r, u); err != nil {
				h.serverError(w, r, err)
				return
			}
			o, err := h.leave.Override(r.Context(), u.ID)
			if err != nil {
				h.serverError(w, r, err)
				return
			}
			row.Override = o != 0
		}
		rows = append(rows, row)
	}
	h.render(w, r, http.StatusOK, "admin_users.html", view{Title: "People", Data: rows})
}

type adminUserData struct {
	User        users.User
	Manager     string
	Approver    string
	OverrideID  int64
	Candidates  []users.User
	Balances    []leave.Balance
	Adjustments []leave.Adjustment
	Requests    []leave.Request
	Errors      leave.FieldErrors
	Adj         leave.Adjustment
	// Types and Rec are the "Record leave" form: every type (a retired one may
	// still be what the leave was), and what
	// was typed when it was refused.
	Types []leave.Type
	Rec   leave.Input
	// RecErrors are kept apart from Errors: both forms have a "type" field.
	RecErrors leave.FieldErrors
	// Teams are read-only here: the directory is the source of truth.
	Teams []leave.Team
}

func (h *Handler) adminUserData(r *http.Request, id int64) (adminUserData, error) {
	ctx := r.Context()
	u, err := h.users.ByID(ctx, id)
	if err != nil {
		return adminUserData{}, err
	}
	d := adminUserData{User: u}
	if u.ManagerDN != "" {
		if m, err := h.users.ByDN(ctx, u.ManagerDN); err == nil {
			d.Manager = m.Name
		}
	}
	if d.Approver, err = h.approverName(r, u); err != nil {
		return d, err
	}
	if d.OverrideID, err = h.leave.Override(ctx, u.ID); err != nil {
		return d, err
	}
	all, err := h.users.Enabled(ctx)
	if err != nil {
		return d, err
	}
	for _, c := range all {
		if c.ID != u.ID {
			d.Candidates = append(d.Candidates, c)
		}
	}
	if d.Balances, err = h.leave.Balances(ctx, u.ID); err != nil {
		return d, err
	}
	if d.Adjustments, err = h.leave.Adjustments(ctx, u.ID); err != nil {
		return d, err
	}
	if d.Types, err = h.leave.Types(ctx, false); err != nil {
		return d, err
	}
	if d.Teams, err = h.leave.TeamsFor(ctx, u.ID); err != nil {
		return d, err
	}
	d.Requests, err = h.leave.ForUser(ctx, u.ID)
	return d, err
}

func (h *Handler) AdminUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d, err := h.adminUserData(r, id)
	if errors.Is(err, users.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_user.html", view{Title: d.User.Name, Data: d})
}

func (h *Handler) AdminSetApprover(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	approver, err := strconv.ParseInt(r.PostFormValue("approver"), 10, 64)
	if err != nil {
		http.Error(w, "Choose an approver, or the directory default.", http.StatusBadRequest)
		return
	}
	if err := h.leave.SetOverride(r.Context(), id, approver); err != nil {
		var fe leave.FieldErrors
		if errors.As(err, &fe) {
			http.Error(w, fe["approver"], http.StatusUnprocessableEntity)
			return
		}
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=saved", id), http.StatusSeeOther)
}

// AdminSetLongWeekend switches whether someone's weekend off includes the
// Friday. The value must be an explicit "0" or "1": an empty post switching
// it either way would be a silent change nobody asked for.
func (h *Handler) AdminSetLongWeekend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var on bool
	switch r.PostFormValue("long_weekend") {
	case "1":
		on = true
	case "0":
	default:
		http.Error(w, "Choose on or off.", http.StatusBadRequest)
		return
	}
	if err := h.users.SetLongWeekend(r.Context(), id, on); err != nil {
		if errors.Is(err, users.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=saved", id), http.StatusSeeOther)
}

func (h *Handler) AdminAddAdjustment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// target is "typeID:cycleStart", one option per balance row: an
	// adjustment always lands on a specific type's specific cycle.
	typePart, cycle, _ := strings.Cut(r.PostFormValue("target"), ":")
	typeID, _ := strconv.ParseInt(typePart, 10, 64)
	days, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue("days")))
	a := leave.Adjustment{UserID: id, TypeID: typeID, CycleStart: cycle,
		Days: days, Reason: r.PostFormValue("reason")}
	err := h.leave.AddAdjustment(r.Context(), currentUser(r).ID, a)
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		d, derr := h.adminUserData(r, id)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		d.Errors, d.Adj = fe, a
		h.render(w, r, http.StatusUnprocessableEntity, "admin_user.html", view{Title: d.User.Name, Data: d})
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=saved", id), http.StatusSeeOther)
}

// AdminRecordLeave records leave a person already took, or agreed off the
// system. See leave.Service.RecordLeave for what it relaxes.
func (h *Handler) AdminRecordLeave(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	typeID, _ := strconv.ParseInt(r.PostFormValue("type"), 10, 64)
	in := leave.Input{TypeID: typeID, Start: r.PostFormValue("start"), End: r.PostFormValue("end"),
		Note: r.PostFormValue("note")}
	rec, err := h.leave.RecordLeave(r.Context(), currentUser(r), id, in)
	var fe leave.FieldErrors
	switch {
	case errors.Is(err, users.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.As(err, &fe):
		d, derr := h.adminUserData(r, id)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		d.RecErrors, d.Rec = fe, in
		h.render(w, r, http.StatusUnprocessableEntity, "admin_user.html", view{Title: d.User.Name, Data: d})
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	notice := "recorded"
	if rec.AllowNegative {
		notice = "recorded-negative"
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=%s", id, notice), http.StatusSeeOther)
}

// AdminUnrecord removes leave an admin recorded.
func (h *Handler) AdminUnrecord(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	rec, err := h.leave.Unrecord(r.Context(), currentUser(r), id)
	switch {
	case errors.Is(err, leave.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, leave.ErrNotAllowed):
		notPossible(w, r, id)
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?notice=unrecorded", rec.UserID), http.StatusSeeOther)
}

// --- Leave types ---------------------------------------------------------

func (h *Handler) AdminTypes(w http.ResponseWriter, r *http.Request) {
	ts, err := h.leave.Types(r.Context(), false)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_types.html", view{Title: "Leave types", Data: ts})
}

type typeFormData struct {
	Type      leave.Type
	Allowance string
	// Backdate is the backdating field as typed, so "abc" comes back as typed.
	Backdate string
	Errors   leave.FieldErrors
}

func (h *Handler) AdminTypeForm(w http.ResponseWriter, r *http.Request) {
	d := typeFormData{Type: leave.Type{Active: true, CycleMonths: 12, Unit: leave.UnitWorkdays,
		Restricted: leave.RestrictMin, CoverageRule: leave.CoverageLimit, BackdateDays: leave.DefaultBackdateDays}}
	if r.PathValue("id") != "" {
		id, ok := pathID(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		t, err := h.leave.Type(r.Context(), id)
		if errors.Is(err, leave.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		d.Type = t
	}
	if d.Type.Allowance != nil {
		d.Allowance = strconv.Itoa(*d.Type.Allowance)
	}
	d.Backdate = strconv.Itoa(d.Type.BackdateDays)
	h.render(w, r, http.StatusOK, "admin_type.html", view{Title: "Leave type", Data: d})
}

func (h *Handler) AdminSaveType(w http.ResponseWriter, r *http.Request) {
	f := r.PostFormValue
	id, _ := strconv.ParseInt(f("id"), 10, 64)
	cycle, _ := strconv.Atoi(f("cycle_months"))
	sort, _ := strconv.Atoi(f("sort"))
	t := leave.Type{ID: id, Code: f("code"), Name: f("name"), CycleMonths: cycle, Unit: f("unit"),
		RequiresNote: f("requires_note") == "1", Active: f("active") == "1", Sort: sort,
		Restricted: f("restricted"), CoverageRule: f("coverage_rule")}
	d := typeFormData{Type: t, Allowance: strings.TrimSpace(f("allowance")), Backdate: strings.TrimSpace(f("backdate_days"))}
	// Parse the numbers first, then validate everything in one pass, so a bad
	// number does not hide a missing name until the next submit.
	parseErrs := leave.FieldErrors{}
	if n, ok := parseWhole(d.Backdate); ok {
		t.BackdateDays, d.Type.BackdateDays = n, n
	} else {
		parseErrs["backdate_days"] = leave.BackdateMessage
	}
	if d.Allowance != "" {
		n, err := strconv.Atoi(d.Allowance)
		if err != nil {
			parseErrs["allowance"] = "Enter a whole number of days, or leave it empty for no limit."
		} else {
			t.Allowance, d.Type.Allowance = &n, &n
		}
	}
	if len(parseErrs) > 0 {
		d.Errors = leave.ValidateType(&t)
		for k, v := range parseErrs {
			d.Errors[k] = v
		}
		h.render(w, r, http.StatusUnprocessableEntity, "admin_type.html", view{Title: "Leave type", Data: d})
		return
	}
	_, err := h.leave.SaveType(r.Context(), t)
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		d.Errors = fe
		h.render(w, r, http.StatusUnprocessableEntity, "admin_type.html", view{Title: "Leave type", Data: d})
		return
	}
	if errors.Is(err, leave.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/types?notice=saved", http.StatusSeeOther)
}

// --- Holidays ------------------------------------------------------------

type holidaysData struct {
	Year     int
	Holidays []leave.HolidayRow
	Errors   leave.FieldErrors
	Date     string
	Name     string
}

func yearParam(s string) int {
	y, err := strconv.Atoi(s)
	if err != nil || y < 2000 || y > 2100 {
		return time.Now().Year()
	}
	return y
}

func (h *Handler) holidaysPage(w http.ResponseWriter, r *http.Request, status int, d holidaysData) {
	var err error
	if d.Holidays, err = h.leave.HolidaysInYear(r.Context(), d.Year); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, status, "admin_holidays.html", view{Title: "Public holidays", Data: d})
}

func (h *Handler) AdminHolidays(w http.ResponseWriter, r *http.Request) {
	h.holidaysPage(w, r, http.StatusOK, holidaysData{Year: yearParam(r.URL.Query().Get("year"))})
}

func (h *Handler) AdminAddHoliday(w http.ResponseWriter, r *http.Request) {
	date, name := r.PostFormValue("date"), r.PostFormValue("name")
	err := h.leave.AddHoliday(r.Context(), date, name)
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		h.holidaysPage(w, r, http.StatusUnprocessableEntity, holidaysData{Year: yearParam(r.PostFormValue("year")),
			Errors: fe, Date: date, Name: name})
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	t, _ := leave.ParseDate(date)
	http.Redirect(w, r, fmt.Sprintf("/admin/holidays?year=%d&notice=saved", t.Year()), http.StatusSeeOther)
}

func (h *Handler) AdminGenerateHolidays(w http.ResponseWriter, r *http.Request) {
	y := yearParam(r.PostFormValue("year"))
	if err := h.leave.GenerateHolidays(r.Context(), y); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/holidays?year=%d&notice=holidays-generated", y), http.StatusSeeOther)
}

func (h *Handler) AdminDeleteHoliday(w http.ResponseWriter, r *http.Request) {
	date := r.PostFormValue("date")
	t, err := leave.ParseDate(date)
	if err != nil {
		http.Error(w, "Not a date.", http.StatusBadRequest)
		return
	}
	if err := h.leave.DeleteHoliday(r.Context(), date); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/holidays?year=%d&notice=saved", t.Year()), http.StatusSeeOther)
}

// --- Outbox --------------------------------------------------------------

type outboxRow struct {
	Job     outbox.Job
	Summary string
}

func (h *Handler) AdminOutbox(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.outbox.Problems(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows := make([]outboxRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, outboxRow{Job: j, Summary: outbox.Summary(j)})
	}
	h.render(w, r, http.StatusOK, "admin_outbox.html", view{Title: "Outbox", Data: rows})
}

func (h *Handler) AdminRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.outbox.Retry(r.Context(), id); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.kick()
	http.Redirect(w, r, "/admin/outbox?notice=retried", http.StatusSeeOther)
}
