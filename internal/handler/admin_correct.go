package handler

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/17xande-dev/leave/internal/leave"
)

// The admin's correction form is the apply page again (apply.html, with
// CorrectID set), so there is one set of fields and one parse path
// (inputFrom). What differs is who the form is for: the types, the picker's
// marks and the preview are the request's owner's, half days are always
// offered, and a reason and the fingerprint the admin read travel with it.

// correctData builds the form for admin correcting req.
func (h *Handler) correctData(r *http.Request, req leave.Request, in leave.Input, errs leave.FieldErrors, reason string) (applyData, error) {
	ctx := r.Context()
	owner, err := h.users.ByID(ctx, req.UserID)
	if err != nil {
		return applyData{}, err
	}
	types, err := h.leave.Types(ctx, false)
	if err != nil {
		return applyData{}, err
	}
	d := applyData{HalfDaysEnabled: true, Input: in, Errors: errs, Preview: quoteData{Incomplete: true},
		Mins: map[int64]string{}, Labels: map[int64]string{}, CorrectID: req.ID, Owner: owner.Name,
		Status: strings.ToLower(statusLabel(req.Status)), Seen: req.Fingerprint(), Reason: reason}
	for _, t := range types {
		// Everything the person could be given, retired types only when it is
		// the request's own: a retired type cannot be newly chosen.
		if !t.OfferedTo(owner) || (!t.Active && t.ID != req.TypeID) {
			continue
		}
		d.Types = append(d.Types, t)
		d.Labels[t.ID] = t.Name
		if !t.StaffCanApply {
			d.Labels[t.ID] += " (admin only)"
		}
	}
	mine, err := h.leave.ForUser(ctx, owner.ID)
	if err != nil {
		return applyData{}, err
	}
	mine = slices.DeleteFunc(mine, func(m leave.Request) bool { return m.ID == req.ID })
	if d.Marks, err = h.pickerMarks(ctx, mine); err != nil {
		return applyData{}, err
	}
	if in.TypeID != 0 && (len(in.Ranges) > 0 || (in.Start != "" && in.End != "")) {
		d.Preview, err = h.correctPreview(r, req, in)
		if err != nil {
			return applyData{}, err
		}
		// The field errors sit beside their fields; the preview keeps only the day count.
		d.Preview.Errors = leave.FieldErrors{}
	}
	d.Approver = "the leave administrators"
	if req.ApproverID != 0 {
		if a, err := h.users.ByID(ctx, req.ApproverID); err == nil {
			d.Approver = a.Name
		}
	}
	if req.Status != leave.StatusPending {
		d.Approver = "HR"
	}
	return d, nil
}

// correctPreview is the live preview of a correction: the quote for the owner
// and the old figure beside the new one.
func (h *Handler) correctPreview(r *http.Request, req leave.Request, in leave.Input) (quoteData, error) {
	qt, old, err := h.leave.PreviewCorrect(r.Context(), currentUser(r), req.ID, in)
	var fe leave.FieldErrors
	if err != nil && !errors.As(err, &fe) {
		return quoteData{}, err
	}
	d := newQuoteData(&qt, fe, true)
	d.CanOverride = false // a correction waives coverage; nothing to ask for
	if qt.Days != 0 {
		d.Was = fmt.Sprintf("Was %s %s; now %s %s.", old.Days, unitLabel(old.Days, old.TypeUnit), qt.Days, unitLabel(qt.Days, qt.Type.Unit))
	}
	return d, nil
}

// correctable loads the request for the correction routes. A request the
// admin cannot correct goes back to its page with the reason: their own, or
// one that is approved, decided or generated.
func (h *Handler) correctable(w http.ResponseWriter, r *http.Request) (leave.Request, bool) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return req, false
	}
	if !leave.CanCorrect(currentUser(r), req) {
		notice := "not-possible"
		if req.UserID == currentUser(r).ID {
			notice = "correct-own"
		}
		http.Redirect(w, r, requestURL(r, req.ID, notice), http.StatusSeeOther)
		return req, false
	}
	return req, true
}

func (h *Handler) AdminCorrectForm(w http.ResponseWriter, r *http.Request) {
	req, ok := h.correctable(w, r)
	if !ok {
		return
	}
	var errs leave.FieldErrors
	blocked, err := h.leave.CorrectBlocked(r.Context(), req)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if blocked != "" {
		errs = leave.FieldErrors{"correct": blocked}
	}
	in := leave.Input{TypeID: req.TypeID, Ranges: req.Ranges, Note: req.Note, HalfStart: req.HalfStart, HalfEnd: req.HalfEnd}
	d, err := h.correctData(r, req, in, errs, "")
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "apply.html", view{Title: "Correct request", Data: d})
}

// AdminCorrectQuote is the form's htmx preview.
func (h *Handler) AdminCorrectQuote(w http.ResponseWriter, r *http.Request) {
	req, ok := h.correctable(w, r)
	if !ok {
		return
	}
	in, parseErr := inputFrom(r)
	if unreadableInput(w, parseErr) {
		return
	}
	if parseErr != nil {
		h.renderPartial(w, r, "quote", quoteData{Errors: parseErr.(leave.FieldErrors)})
		return
	}
	if in.TypeID == 0 || (len(in.Ranges) == 0 && in.Start == "") {
		h.renderPartial(w, r, "quote", quoteData{Incomplete: true})
		return
	}
	if len(in.Ranges) == 0 && in.End == "" {
		in.End = in.Start
	}
	d, err := h.correctPreview(r, req, in)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	delete(d.Errors, "note")
	h.renderPartial(w, r, "quote", d)
}

func (h *Handler) AdminCorrect(w http.ResponseWriter, r *http.Request) {
	req, ok := h.correctable(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	in, err := inputFrom(r)
	if unreadableInput(w, err) {
		return
	}
	// The form always sends the fingerprint it was built from; without it the
	// admin could overwrite a change they never saw.
	seen := r.PostForm.Get("seen")
	if !fingerprintRE.MatchString(seen) {
		http.Error(w, "That form could not be read.", http.StatusBadRequest)
		return
	}
	reason := r.PostForm.Get("reason")
	if err == nil {
		_, err = h.leave.Correct(r.Context(), currentUser(r), req.ID, in, reason, seen)
	}
	var fe leave.FieldErrors
	switch {
	case errors.Is(err, leave.ErrChanged):
		http.Redirect(w, r, requestURL(r, req.ID, "changed"), http.StatusSeeOther)
	case errors.As(err, &fe):
		// Show what was typed against the request as it stands now.
		d, derr := h.correctData(r, req, in, fe, reason)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		// Keep the fingerprint the admin actually read, not the current one:
		// refreshing it here would let the next post overwrite an edit the
		// admin never saw.
		d.Seen = seen
		h.render(w, r, http.StatusUnprocessableEntity, "apply.html", view{Title: "Correct request", Data: d})
	case errors.Is(err, leave.ErrNotAllowed):
		notPossible(w, r, req.ID)
	case err != nil:
		h.serverError(w, r, err)
	default:
		http.Redirect(w, r, requestURL(r, req.ID, "corrected"), http.StatusSeeOther)
	}
}
