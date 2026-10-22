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
	// Drafts are requests waiting for the person to submit them: shutdown
	// requests created for them, and ones HR allowed to go below zero.
	Drafts []leave.Request
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
	drafts, err := h.leave.Drafts(r.Context(), u.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "home.html", view{Title: "My leave",
		Data: homeData{Balances: bs, Requests: rs, Drafts: drafts}})
}

type applyData struct {
	Types    []leave.Type
	Input    leave.Input
	Errors   leave.FieldErrors
	Preview  quoteData
	Approver string
	// MinDate is the picker's earliest selectable day: the selected type's
	// cut-off, or with none chosen the earliest across all offered types.
	MinDate string
	// Mins maps a type's ID to its earliest start (YYYY-MM-DD), for the
	// select's data-min attributes.
	Mins map[int64]string
}

func (h *Handler) applyData(r *http.Request, in leave.Input, errs leave.FieldErrors) (applyData, error) {
	types, err := h.leave.Types(r.Context(), true)
	if err != nil {
		return applyData{}, err
	}
	d := applyData{Types: types, Input: in, Errors: errs, Preview: quoteData{Incomplete: true}, Mins: map[int64]string{}}
	// The same cut-off the server enforces (Service.EarliestStart), so the
	// picker greys out exactly what a submit would refuse. YYYY-MM-DD
	// compares as text.
	for _, t := range types {
		m := h.leave.EarliestStart(t)
		d.Mins[t.ID] = m
		if d.MinDate == "" || m < d.MinDate {
			d.MinDate = m
		}
	}
	if m, ok := d.Mins[in.TypeID]; ok {
		d.MinDate = m
	}
	// Re-rendering after a refused submit: show the day count too, so the
	// form reads the same as it did before the button was pressed. The field
	// errors are already beside their fields; only the coverage warning, which
	// has no field, is shown with the preview.
	if in.TypeID != 0 && in.Start != "" && in.End != "" {
		qt, err := h.leave.Preview(r.Context(), currentUser(r), in)
		var fe leave.FieldErrors
		if err == nil || errors.As(err, &fe) {
			d.Preview = newQuoteData(&qt, fe)
			d.Preview.Errors = leave.FieldErrors{}
			if msg := fe["coverage"]; msg != "" {
				d.Preview.Errors["coverage"] = msg
			}
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
		End: strings.TrimSpace(get("end")), Note: get("note"),
		Override: get("override") == "1", Negative: get("negative") == "1"}
}

// quoteData is the live preview under the apply form.
type quoteData struct {
	Quote  *leave.Quote
	Errors leave.FieldErrors
	// Incomplete: the form is not filled in far enough to say anything yet.
	Incomplete bool
	// CanOverride offers special permission: the request is over a department
	// limit and nothing else stands in its way but the balance. CanNegative
	// offers asking HR to go below zero: the balance is the only thing wrong
	// (a coverage limit does not matter until the request is submitted).
	CanOverride, CanNegative bool
}

// newQuoteData reads what a refused quote leaves open to the person; see
// leave.Offers.
func newQuoteData(qt *leave.Quote, fe leave.FieldErrors) quoteData {
	d := quoteData{Quote: qt, Errors: fe}
	d.CanOverride, d.CanNegative = leave.Offers(*qt, fe)
	return d
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
	h.renderPartial(w, r, "quote", newQuoteData(&qt, fe))
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
	notice := "submitted"
	if req.Status == leave.StatusAwaitingHR {
		notice = "negative-requested"
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=%s", req.ID, notice), http.StatusSeeOther)
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
	CanSubmit           bool
	// Coverage is the department and team warnings for a pending request
	// needing special permission, as they stand now. Next names who it goes to if the viewer
	// approves it; empty when their approval is the last.
	Coverage []leave.Coverage
	Next     string
	// Steps is the trail of decisions on the request.
	Steps []leave.Step
	// Shortfall is how far below zero a request awaiting HR would go.
	Shortfall int
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
		CanWithdraw: leave.CanWithdraw(u, req),
		CanCancel:   h.leave.CanRequestCancellation(u, req),
		CanDecide:   leave.CanDecide(u, req),
		CanSubmit:   req.UserID == u.ID && req.Status == leave.StatusDraft,
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
	var err error
	if d.Steps, err = h.leave.Steps(r.Context(), req.ID); err != nil {
		return d, err
	}
	if req.Escalate && req.Status == leave.StatusPending {
		if d.Coverage, err = h.leave.CoverageFor(r.Context(), req); err != nil {
			return d, err
		}
		if d.CanDecide {
			next, ok, err := h.leave.NextApprover(r.Context(), u, req)
			if err != nil {
				return d, err
			}
			if ok {
				d.Next = next.Name
			}
		}
	}
	if req.Status == leave.StatusAwaitingHR {
		if d.Shortfall, err = h.leave.Shortfall(r.Context(), req); err != nil {
			return d, err
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

// SubmitDraft submits a draft: a shutdown request someone was given
// pre-filled, or one HR allowed to go below zero. override asks for special
// permission when the second kind is over a coverage limit.
func (h *Handler) SubmitDraft(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	note := r.PostFormValue("note")
	out, err := h.leave.SubmitDraftOverride(r.Context(), currentUser(r), req.ID, note,
		r.PostFormValue("override") == "1")
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		// Overlap with leave booked separately: the person needs to sort that
		// out (withdraw it, or ask an admin) before this can go in.
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
	notice := "submitted"
	if out.Status == leave.StatusApproved {
		notice = "approved"
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=%s", out.ID, notice), http.StatusSeeOther)
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
	decided, err := h.leave.Decide(r.Context(), currentUser(r), req.ID, approve, note)
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
		if decided.Status == leave.StatusPending {
			notice = "escalated" // moved up a level rather than finished
		}
	}
	// Back to the queue if that is where the decision was made from.
	if r.PostFormValue("from") == "queue" {
		http.Redirect(w, r, "/approvals?notice="+notice, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/leave/%d?notice=%s", req.ID, notice), http.StatusSeeOther)
}

// queueRow is a request in the approvals queue with what an approver needs to
// weigh it: for one needing special permission, the coverage warnings as they
// stands now, and who it would go to next.
type queueRow struct {
	leave.Request
	Coverage []leave.Coverage
	Next     string
}

type queueData struct {
	Queue []queueRow
}

func (h *Handler) Approvals(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q, err := h.leave.Queue(r.Context(), u)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows := make([]queueRow, 0, len(q))
	for _, req := range q {
		row := queueRow{Request: req}
		if req.Escalate {
			if row.Coverage, err = h.leave.CoverageFor(r.Context(), req); err != nil {
				h.serverError(w, r, err)
				return
			}
			next, ok, err := h.leave.NextApprover(r.Context(), u, req)
			if err != nil {
				h.serverError(w, r, err)
				return
			}
			if ok {
				row.Next = next.Name
			}
		}
		rows = append(rows, row)
	}
	h.render(w, r, http.StatusOK, "approvals.html", view{Title: "Approvals", Data: queueData{Queue: rows}})
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
	case leave.StatusPending, leave.StatusDraft, leave.StatusAwaitingHR:
		return "pending"
	case leave.StatusApproved:
		return "done"
	case leave.StatusDeclined, leave.StatusCancelled:
		return "problem"
	}
	return ""
}

func statusLabel(s string) string {
	if s == leave.StatusAwaitingHR {
		return "awaiting HR"
	}
	return s
}
