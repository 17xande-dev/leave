package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// The pages for naming a stand-in while an approver is away. Everyone may set
// up their own (the page is linked only where it is likely to matter, but it is
// open to all); an admin may also do it for anyone from the person's page. The
// rules (who may be named, the dates, one at a time) live in the service.

// delegationInput is the form as typed, so a refused one comes back filled.
type delegationInput struct {
	Delegate         int64
	Start, End, Note string
}

// delegationPanel is what the shared form and list partials render.
type delegationPanel struct {
	Heading, Hint string
	// Action is where the form posts; RevokeBase prefixes each row's revoke
	// path ("/delegations", "/admin/delegations"); From is a hidden field that
	// sends an admin's revoke back to the person's page.
	Action, RevokeBase, From string
	CSRF                     string
	// IDP prefixes the form's element ids, so the person page's other forms
	// keep theirs unique.
	IDP           string
	People        []users.User
	In            delegationInput
	Errors        leave.FieldErrors
	Today         string
	Current, Past []leave.Delegation
	// ShowDelegator adds the approver to the lists, for pages about everyone.
	ShowDelegator bool
}

type delegationsData struct {
	Panel    delegationPanel
	MaxDays  int
	TodayStr string
	// ToYou is who the viewer stands in for, now or later.
	ToYou []leave.Delegation
}

// standInChoices is everyone who can be named for subject: enabled people but
// the subject themselves.
func (h *Handler) standInChoices(r *http.Request, subject users.User) ([]users.User, error) {
	all, err := h.users.Enabled(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]users.User, 0, len(all))
	for _, u := range all {
		if u.ID != subject.ID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (h *Handler) delegationPanel(r *http.Request, subject users.User, in delegationInput, errs leave.FieldErrors) (delegationPanel, error) {
	people, err := h.standInChoices(r, subject)
	if err != nil {
		return delegationPanel{}, err
	}
	cur, err := h.leave.Delegations(r.Context(), leave.DelegationFilter{DelegatorID: subject.ID})
	if err != nil {
		return delegationPanel{}, err
	}
	past, err := h.leave.Delegations(r.Context(), leave.DelegationFilter{DelegatorID: subject.ID, Past: true})
	if err != nil {
		return delegationPanel{}, err
	}
	return delegationPanel{CSRF: csrfToken(r), People: people, In: in, Errors: errs,
		Today: h.leave.Today().Format(time.DateOnly), Current: cur, Past: past}, nil
}

func (h *Handler) delegationsData(r *http.Request, in delegationInput, errs leave.FieldErrors) (delegationsData, error) {
	u := currentUser(r)
	p, err := h.delegationPanel(r, u, in, errs)
	if err != nil {
		return delegationsData{}, err
	}
	p.Heading, p.Action, p.RevokeBase = "Name a stand-in", "/delegations", "/delegations"
	toYou, err := h.leave.Delegations(r.Context(), leave.DelegationFilter{DelegateID: u.ID})
	if err != nil {
		return delegationsData{}, err
	}
	return delegationsData{Panel: p, MaxDays: leave.MaxDelegationDays, TodayStr: p.Today, ToYou: toYou}, nil
}

// delegationNotice is the notice after a stand-in is saved. It names the
// stand-in and the dates, which the fixed map cannot, so the redirect carries
// the delegation's id and the text is built here from the stored row, never
// from the query. Someone who may not see that delegation gets the plain notice.
func (h *Handler) delegationNotice(r *http.Request) string {
	if r.URL.Query().Get("notice") != "delegated" {
		return ""
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("d"), 10, 64)
	if err != nil {
		return ""
	}
	d, err := h.leave.Delegation(r.Context(), id)
	u := currentUser(r)
	if err != nil || (d.DelegatorID != u.ID && !u.Admin) {
		return ""
	}
	who := "you"
	if d.DelegatorID != u.ID {
		who = d.DelegatorName
	}
	return fmt.Sprintf("%s will approve for %s from %s to %s. They have been emailed.", d.DelegateName, who, dateText(d.Start), dateText(d.End))
}

func dateText(day string) string {
	t, err := leave.ParseDate(day)
	if err != nil {
		return day
	}
	return t.Format("Mon 2 Jan 2006")
}

// Delegations is the signed-in person's own page.
func (h *Handler) Delegations(w http.ResponseWriter, r *http.Request) {
	d, err := h.delegationsData(r, delegationInput{}, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "delegations.html", view{Title: "Approvals while you are away", Notice: h.delegationNotice(r), Data: d})
}

func delegationFrom(r *http.Request) delegationInput {
	delegate, _ := strconv.ParseInt(r.PostFormValue("delegate"), 10, 64)
	return delegationInput{Delegate: delegate, Start: r.PostFormValue("start"), End: r.PostFormValue("end"), Note: r.PostFormValue("note")}
}

// delegate saves a stand-in for subject, whatever the page. A refusal is
// returned for the caller to re-render with (ok false, fe set); a stand-in the
// actor may not set at all is a 403-like not-possible.
func (h *Handler) saveDelegation(w http.ResponseWriter, r *http.Request, subject users.User) (leave.Delegation, leave.FieldErrors, delegationInput, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form could not be read.", http.StatusBadRequest)
		return leave.Delegation{}, nil, delegationInput{}, false
	}
	in := delegationFrom(r)
	d, err := h.leave.Delegate(r.Context(), currentUser(r), subject.ID, in.Delegate, in.Start, in.End, in.Note)
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		return leave.Delegation{}, fe, in, false
	case errors.Is(err, leave.ErrNotAllowed):
		http.NotFound(w, r)
		return leave.Delegation{}, nil, in, false
	case err != nil:
		h.serverError(w, r, err)
		return leave.Delegation{}, nil, in, false
	}
	return d, nil, in, true
}

// CreateDelegation names a stand-in for the signed-in person.
func (h *Handler) CreateDelegation(w http.ResponseWriter, r *http.Request) {
	d, fe, in, ok := h.saveDelegation(w, r, currentUser(r))
	if !ok {
		if fe == nil {
			return // saveDelegation answered
		}
		data, err := h.delegationsData(r, in, fe)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		h.render(w, r, http.StatusUnprocessableEntity, "delegations.html", view{Title: "Approvals while you are away", Data: data})
		return
	}
	http.Redirect(w, r, "/delegations?notice=delegated&d="+strconv.FormatInt(d.ID, 10), http.StatusSeeOther)
}

// revoke ends the delegation in the path. Someone who is neither its delegator
// nor an admin gets a 404, as for an id that does not exist: it is not
// confirmed to them. One already over or revoked is told so.
func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) (leave.Delegation, bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return leave.Delegation{}, false
	}
	u := currentUser(r)
	d, err := h.leave.Delegation(r.Context(), id)
	if errors.Is(err, leave.ErrNotFound) || (err == nil && d.DelegatorID != u.ID && !u.Admin) {
		http.NotFound(w, r)
		return leave.Delegation{}, false
	}
	if err != nil {
		h.serverError(w, r, err)
		return leave.Delegation{}, false
	}
	switch _, err := h.leave.RevokeDelegation(r.Context(), u, id); {
	case errors.Is(err, leave.ErrNotAllowed):
		http.Redirect(w, r, backTo(r, d)+"?notice=delegation-over", http.StatusSeeOther)
		return d, false
	case err != nil:
		h.serverError(w, r, err)
		return d, false
	}
	return d, true
}

// RevokeDelegation ends one of the signed-in person's stand-ins.
func (h *Handler) RevokeDelegation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.revoke(w, r); ok {
		http.Redirect(w, r, "/delegations?notice=delegation-revoked", http.StatusSeeOther)
	}
}

// backTo is where an admin's revoke returns: the person's page when it came
// from there, else the list of all stand-ins. The path is built here, never
// taken from the form.
func backTo(r *http.Request, d leave.Delegation) string {
	if !strings.HasPrefix(r.URL.Path, "/admin/") {
		return "/delegations"
	}
	if r.PostFormValue("from") == "user" {
		return "/admin/users/" + strconv.FormatInt(d.DelegatorID, 10)
	}
	return "/admin/delegations"
}

// --- Admin ---------------------------------------------------------------

type adminDelegationsData struct {
	Panel delegationPanel
}

// AdminDelegations lists every current and upcoming stand-in.
func (h *Handler) AdminDelegations(w http.ResponseWriter, r *http.Request) {
	cur, err := h.leave.Delegations(r.Context(), leave.DelegationFilter{})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	past, err := h.leave.Delegations(r.Context(), leave.DelegationFilter{Past: true})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	p := delegationPanel{CSRF: csrfToken(r), RevokeBase: "/admin/delegations", Current: cur, Past: past, ShowDelegator: true}
	h.render(w, r, http.StatusOK, "admin_delegations.html", view{Title: "Stand-ins", Notice: h.delegationNotice(r), Data: adminDelegationsData{Panel: p}})
}

// AdminCreateDelegation names a stand-in for the person in the path.
func (h *Handler) AdminCreateDelegation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	subject, err := h.users.ByID(r.Context(), id)
	if errors.Is(err, users.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d, fe, in, ok := h.saveDelegation(w, r, subject)
	if !ok {
		if fe == nil {
			return
		}
		data, err := h.adminUserData(r, id)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		data.Stand.In, data.Stand.Errors = in, fe
		h.render(w, r, http.StatusUnprocessableEntity, "admin_user.html", view{Title: data.User.Name, Data: data})
		return
	}
	http.Redirect(w, r, "/admin/users/"+strconv.FormatInt(id, 10)+"?notice=delegated&d="+strconv.FormatInt(d.ID, 10), http.StatusSeeOther)
}

// AdminRevokeDelegation ends any stand-in.
func (h *Handler) AdminRevokeDelegation(w http.ResponseWriter, r *http.Request) {
	d, ok := h.revoke(w, r)
	if !ok {
		return
	}
	http.Redirect(w, r, backTo(r, d)+"?notice=delegation-revoked", http.StatusSeeOther)
}
