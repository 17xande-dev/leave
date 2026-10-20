package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

type homeData struct {
	Balances []leave.Balance
	Requests []leave.Request
}

func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	bs, err := h.leave.Balances(r.Context(), u.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rs, err := h.leave.ForUser(r.Context(), u.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "home.html", view{Title: "My leave", Data: homeData{Balances: bs, Requests: rs}})
}

type applyData struct {
	Types    []leave.Type
	Input    leave.Input
	Errors   leave.FieldErrors
	Preview  quoteData
	Approver string
}

func (h *Handler) applyData(r *http.Request, in leave.Input, errs leave.FieldErrors) (applyData, error) {
	types, err := h.leave.Types(r.Context(), true)
	if err != nil {
		return applyData{}, err
	}
	d := applyData{Types: types, Input: in, Errors: errs, Preview: quoteData{Incomplete: true}}
	// Re-rendering after a refused submit: show the day count too, so the
	// form reads the same as it did before the button was pressed.
	if in.TypeID != 0 && in.Start != "" && in.End != "" {
		if qt, err := h.leave.Preview(r.Context(), currentUser(r), in); err == nil || errors.As(err, new(leave.FieldErrors)) {
			d.Preview = quoteData{Quote: &qt}
		}
	}
	id, err := h.leave.ResolveApprover(r.Context(), currentUser(r))
	if err != nil {
		return applyData{}, err
	}
	d.Approver = "the leave administrators"
	if id != 0 {
		if a, err := h.users.ByID(r.Context(), id); err == nil {
			d.Approver = a.Name
		}
	}
	return d, nil
}

func (h *Handler) ApplyForm(w http.ResponseWriter, r *http.Request) {
	d, err := h.applyData(r, leave.Input{}, nil)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "apply.html", view{Title: "Apply for leave", Data: d})
}

func inputFrom(r *http.Request, get func(string) string) leave.Input {
	id, _ := strconv.ParseInt(get("type"), 10, 64)
	return leave.Input{TypeID: id, Start: strings.TrimSpace(get("start")),
		End: strings.TrimSpace(get("end")), Note: get("note")}
}

// quoteData is the live preview under the apply form.
type quoteData struct {
	Quote  *leave.Quote
	Errors leave.FieldErrors
	// Incomplete: the form is not filled in far enough to say anything yet.
	Incomplete bool
}

// Quote renders the day count and balance for the dates on the apply form.
// It is an htmx convenience: Apply repeats every check, and the form works
// without it.
func (h *Handler) Quote(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := inputFrom(r, q.Get)
	if in.TypeID == 0 || in.Start == "" {
		h.renderPartial(w, r, "quote", quoteData{Incomplete: true})
		return
	}
	if in.End == "" {
		in.End = in.Start
	}
	qt, err := h.leave.Preview(r.Context(), currentUser(r), in)
	var fe leave.FieldErrors
	if err != nil && !errors.As(err, &fe) {
		h.serverError(w, r, err)
		return
	}
	// A missing note is not worth shouting about while someone is still
	// picking dates; Apply will say so if it is still missing.
	delete(fe, "note")
	h.renderPartial(w, r, "quote", quoteData{Quote: &qt, Errors: fe})
}

func (h *Handler) Apply(w http.ResponseWriter, r *http.Request) {
	in := inputFrom(r, r.PostFormValue)
	req, err := h.leave.Submit(r.Context(), currentUser(r), in)
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		d, derr := h.applyData(r, in, fe)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		h.render(w, r, http.StatusUnprocessableEntity, "apply.html", view{Title: "Apply for leave", Data: d})
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=submitted", req.ID), http.StatusSeeOther)
}

type requestData struct {
	Request leave.Request
	// Original is the leave a cancellation request is for.
	Original *leave.Request
	// PendingCancellation is a cancellation waiting on this leave.
	PendingCancellation *leave.Request
	CanWithdraw         bool
	CanCancel           bool
	CanDecide           bool
	// Candidates is who an admin may reassign a pending request to.
	Candidates []users.User
	Errors     leave.FieldErrors
	Note       string
}

func (h *Handler) loadRequest(w http.ResponseWriter, r *http.Request) (leave.Request, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return leave.Request{}, false
	}
	req, err := h.leave.Get(r.Context(), id)
	// Someone else's request is a 404, not a 403: its existence is not
	// something to confirm to a person who cannot see it.
	if errors.Is(err, leave.ErrNotFound) || (err == nil && !leave.CanView(currentUser(r), req)) {
		http.NotFound(w, r)
		return leave.Request{}, false
	}
	if err != nil {
		h.serverError(w, r, err)
		return leave.Request{}, false
	}
	return req, true
}

func (h *Handler) requestData(r *http.Request, req leave.Request) (requestData, error) {
	u := currentUser(r)
	d := requestData{
		Request:     req,
		CanWithdraw: req.UserID == u.ID && req.Status == leave.StatusPending,
		CanCancel:   h.leave.CanRequestCancellation(u, req),
		CanDecide:   leave.CanDecide(u, req),
	}
	if u.Admin && req.Status == leave.StatusPending {
		all, err := h.users.Enabled(r.Context())
		if err != nil {
			return d, err
		}
		for _, c := range all {
			if c.ID != req.UserID {
				d.Candidates = append(d.Candidates, c)
			}
		}
	}
	if req.IsCancellation() {
		o, err := h.leave.Get(r.Context(), req.CancelsID)
		if err != nil {
			return d, err
		}
		d.Original = &o
	} else if req.Status == leave.StatusApproved {
		pc, err := h.leave.PendingCancellation(r.Context(), req.ID)
		if err != nil {
			return d, err
		}
		d.PendingCancellation = pc
		d.CanCancel = d.CanCancel && pc == nil
	}
	return d, nil
}

func (h *Handler) RequestPage(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	d, err := h.requestData(r, req)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "request.html", view{Title: requestTitle(req), Data: d})
}

func requestTitle(r leave.Request) string {
	if r.IsCancellation() {
		return "Cancellation of " + strings.ToLower(r.TypeName)
	}
	return r.TypeName
}

// notPossible answers a transition the request's state no longer allows --
// typically a second tab, or someone else deciding first.
func notPossible(w http.ResponseWriter, r *http.Request, id int64) {
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=not-possible", id), http.StatusSeeOther)
}

func (h *Handler) Withdraw(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	err := h.leave.Withdraw(r.Context(), currentUser(r), req.ID)
	if errors.Is(err, leave.ErrNotAllowed) {
		notPossible(w, r, req.ID)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=withdrawn", req.ID), http.StatusSeeOther)
}

func (h *Handler) RequestCancellation(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	c, err := h.leave.RequestCancellation(r.Context(), currentUser(r), req.ID, r.PostFormValue("note"))
	if errors.Is(err, leave.ErrNotAllowed) {
		notPossible(w, r, req.ID)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=cancel-requested", c.ID), http.StatusSeeOther)
}

// Decide approves or declines. The decision is an explicit "approve" or
// "decline"; anything else -- including an empty body -- is a 400 rather than
// quietly meaning one of them.
func (h *Handler) Decide(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	var approve bool
	switch r.PostFormValue("decision") {
	case "approve":
		approve = true
	case "decline":
	default:
		http.Error(w, "Choose approve or decline.", http.StatusBadRequest)
		return
	}
	note := r.PostFormValue("note")
	_, err := h.leave.Decide(r.Context(), currentUser(r), req.ID, approve, note)
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		d, derr := h.requestData(r, req)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		d.Errors, d.Note = fe, note
		h.render(w, r, http.StatusUnprocessableEntity, "request.html", view{Title: requestTitle(req), Data: d})
		return
	case errors.Is(err, leave.ErrNotAllowed):
		notPossible(w, r, req.ID)
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	notice := "declined"
	if approve {
		notice = "approved"
	}
	// Back to the queue if that is where the decision was made from.
	if r.PostFormValue("from") == "queue" {
		http.Redirect(w, r, "/approvals?notice="+notice, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=%s", req.ID, notice), http.StatusSeeOther)
}

type queueData struct {
	Queue []leave.Request
}

func (h *Handler) Approvals(w http.ResponseWriter, r *http.Request) {
	q, err := h.leave.Queue(r.Context(), currentUser(r))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "approvals.html", view{Title: "Approvals", Data: queueData{Queue: q}})
}

func (h *Handler) Team(w http.ResponseWriter, r *http.Request) {
	rs, err := h.leave.Team(r.Context(), currentUser(r))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "team.html", view{Title: "Team leave", Data: rs})
}

// statusClass maps a status to its badge style.
func statusClass(s string) string {
	switch s {
	case leave.StatusPending:
		return "pending"
	case leave.StatusApproved:
		return "done"
	case leave.StatusDeclined, leave.StatusCancelled:
		return "problem"
	}
	return ""
}
