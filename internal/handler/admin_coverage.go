package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/17xande-dev/leave/internal/leave"
)

// --- Coverage limits -----------------------------------------------------

// coverageRow is one line of the coverage page: a department (or, with an empty
// Name, the default rule), its size and its limit.
type coverageRow struct {
	Name    string
	Size    int
	Percent string
	Error   string
}

type coverageData struct {
	Default coverageRow
	Rows    []coverageRow
}

func (h *Handler) coverageData(r *http.Request, failed *coverageRow) (coverageData, error) {
	rules, err := h.leave.CoverageRules(r.Context())
	if err != nil {
		return coverageData{}, err
	}
	depts, err := h.leave.Departments(r.Context())
	if err != nil {
		return coverageData{}, err
	}
	pct := func(name string) string {
		if p, ok := rules[name]; ok {
			return strconv.Itoa(p)
		}
		return ""
	}
	d := coverageData{Default: coverageRow{Percent: pct("")}}
	seen := map[string]bool{}
	for _, dep := range depts {
		seen[dep.Name] = true
		d.Rows = append(d.Rows, coverageRow{Name: dep.Name, Size: dep.Size, Percent: pct(dep.Name)})
	}
	// A rule for a department nobody is in any more stays visible, so it can
	// be removed rather than lingering unseen.
	for name := range rules {
		if name != "" && !seen[name] {
			d.Rows = append(d.Rows, coverageRow{Name: name, Percent: pct(name)})
		}
	}
	// What was typed comes back beside the rule it was for, with its error.
	if failed != nil {
		if failed.Name == "" {
			d.Default = *failed
		}
		for i := range d.Rows {
			if d.Rows[i].Name == failed.Name {
				d.Rows[i].Percent, d.Rows[i].Error = failed.Percent, failed.Error
			}
		}
	}
	return d, nil
}

func (h *Handler) AdminCoverage(w http.ResponseWriter, r *http.Request) {
	d, err := h.coverageData(r, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_coverage.html", view{Title: "Coverage limits", Data: d})
}

// parsePercent reads the limit field strictly: whole digits from 1 to 100, or
// empty to remove the rule (returned as 0). strconv.Atoi alone would accept
// "+5" and "-0".
func parsePercent(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	if len(s) > 3 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 100
}

func (h *Handler) AdminSaveCoverage(w http.ResponseWriter, r *http.Request) {
	dept, raw := r.PostFormValue("department"), r.PostFormValue("percent")
	// Only a department someone is in (or already has a rule) can be limited:
	// a typo in a hand-made post would otherwise store a rule for nobody.
	d, err := h.coverageData(r, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	known := dept == ""
	for _, row := range d.Rows {
		known = known || row.Name == dept
	}
	if !known {
		http.Error(w, "Unknown department.", http.StatusBadRequest)
		return
	}
	n, ok := parsePercent(raw)
	if !ok {
		d, err := h.coverageData(r, &coverageRow{Name: dept, Percent: raw,
			Error: "Enter a whole number from 1 to 100, or leave it empty for no limit."})
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		h.render(w, r, http.StatusUnprocessableEntity, "admin_coverage.html", view{Title: "Coverage limits", Data: d})
		return
	}
	if err := h.leave.SetCoverageRule(r.Context(), dept, n); err != nil {
		var fe leave.FieldErrors
		if errors.As(err, &fe) {
			http.Error(w, fe["percent"], http.StatusBadRequest)
			return
		}
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/coverage?notice=coverage-saved", http.StatusSeeOther)
}

// --- Negative-balance permission -----------------------------------------

type permissionRow struct {
	leave.Request
	Shortfall int
}

type permissionsData struct {
	Rows   []permissionRow
	Errors leave.FieldErrors
	// For is the request whose refusal needed a note.
	For  int64
	Note string
}

func (h *Handler) permissionsPage(w http.ResponseWriter, r *http.Request, status int, d permissionsData) {
	rs, err := h.leave.AwaitingHR(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	for _, req := range rs {
		short, err := h.leave.Shortfall(r.Context(), req)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		d.Rows = append(d.Rows, permissionRow{Request: req, Shortfall: short})
	}
	h.render(w, r, status, "admin_permissions.html", view{Title: "Permissions", Data: d})
}

func (h *Handler) AdminPermissions(w http.ResponseWriter, r *http.Request) {
	h.permissionsPage(w, r, http.StatusOK, permissionsData{})
}

func (h *Handler) AdminGrantNegative(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, err := h.leave.GrantNegative(r.Context(), currentUser(r), id, r.PostFormValue("note"))
	h.permissionResult(w, r, id, "negative-granted", err, r.PostFormValue("note"))
}

func (h *Handler) AdminRefuseNegative(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, err := h.leave.RefuseNegative(r.Context(), currentUser(r), id, r.PostFormValue("note"))
	h.permissionResult(w, r, id, "negative-refused", err, r.PostFormValue("note"))
}

func (h *Handler) permissionResult(w http.ResponseWriter, r *http.Request, id int64, notice string, err error, note string) {
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		h.permissionsPage(w, r, http.StatusUnprocessableEntity, permissionsData{Errors: fe, For: id, Note: note})
	case errors.Is(err, leave.ErrNotAllowed), errors.Is(err, leave.ErrNotFound):
		http.Redirect(w, r, "/admin/permissions?notice=not-possible", http.StatusSeeOther)
	case err != nil:
		h.serverError(w, r, err)
	default:
		http.Redirect(w, r, fmt.Sprintf("/admin/permissions?notice=%s", notice), http.StatusSeeOther)
	}
}
