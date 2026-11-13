package handler

import (
	"errors"
	"net/http"

	"github.com/17xande-dev/leave/internal/leave"
)

func (h *Handler) AdminSetHalves(w http.ResponseWriter, r *http.Request) {
	req, ok := h.loadRequest(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	flags := map[string]bool{}
	for _, key := range []string{"half_start", "half_end"} {
		values := r.PostForm[key]
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 || values[0] != "1" {
			http.Error(w, "Bad half-day checkbox.", http.StatusBadRequest)
			return
		}
		flags[key] = true
	}
	_, err := h.leave.SetHalves(r.Context(), currentUser(r), req.ID, flags["half_start"], flags["half_end"])
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		d, derr := h.requestData(r, req)
		if derr != nil {
			h.serverError(w, r, derr)
			return
		}
		d.HalvesErrors = fe
		d.Request.HalfStart, d.Request.HalfEnd = flags["half_start"], flags["half_end"]
		h.render(w, r, http.StatusUnprocessableEntity, "request.html", view{Title: requestTitle(req), Data: d})
	case errors.Is(err, leave.ErrNotAllowed):
		notPossible(w, r, req.ID)
	case err != nil:
		h.serverError(w, r, err)
	default:
		http.Redirect(w, r, requestURL(r, req.ID, "halves-saved"), http.StatusSeeOther)
	}
}
