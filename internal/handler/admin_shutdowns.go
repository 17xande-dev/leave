package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/17xande-dev/leave/internal/leave"
)

// shutdownFormRows is how many part rows the new-shutdown form offers. Two
// is the usual (annual leave, then company closure); a spare row covers a
// split around a holiday.
const shutdownFormRows = 3

type shutdownFormData struct {
	Types  []leave.Type
	Input  leave.ShutdownInput
	Errors leave.FieldErrors
}

func (h *Handler) shutdownForm(w http.ResponseWriter, r *http.Request, status int, d shutdownFormData) {
	types, err := h.leave.Types(r.Context(), false)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d.Types = types
	for len(d.Input.Parts) < shutdownFormRows {
		d.Input.Parts = append(d.Input.Parts, leave.PartInput{})
	}
	h.render(w, r, status, "admin_shutdown_new.html", view{Title: "New shutdown", Data: d})
}

func (h *Handler) AdminShutdowns(w http.ResponseWriter, r *http.Request) {
	sds, err := h.leave.Shutdowns(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_shutdowns.html", view{Title: "Shutdowns", Data: sds})
}

func (h *Handler) AdminShutdownForm(w http.ResponseWriter, r *http.Request) {
	h.shutdownForm(w, r, http.StatusOK, shutdownFormData{})
}

func (h *Handler) AdminCreateShutdown(w http.ResponseWriter, r *http.Request) {
	in := leave.ShutdownInput{Name: r.PostFormValue("name"), AutoApprove: r.PostFormValue("auto_approve") == "1"}
	for i := range shutdownFormRows {
		n := strconv.Itoa(i)
		typeID, _ := strconv.ParseInt(r.PostFormValue("type"+n), 10, 64)
		in.Parts = append(in.Parts, leave.PartInput{TypeID: typeID,
			Start: r.PostFormValue("start" + n), End: r.PostFormValue("end" + n)})
	}
	id, err := h.leave.CreateShutdown(r.Context(), currentUser(r), in)
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		h.shutdownForm(w, r, http.StatusUnprocessableEntity, shutdownFormData{Input: in, Errors: fe})
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/shutdowns/%d?notice=shutdown-created", id), http.StatusSeeOther)
}

type shutdownData struct {
	Shutdown leave.Shutdown
	Requests []leave.Request
	// Counts by status, for the summary line.
	Drafts, Pending, Approved int
	// Deletable while nobody has submitted anything.
	Deletable bool
}

func (h *Handler) AdminShutdown(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sd, err := h.leave.Shutdown(r.Context(), id)
	if errors.Is(err, leave.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rs, err := h.leave.ShutdownRequests(r.Context(), id)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	d := shutdownData{Shutdown: sd, Requests: rs, Deletable: true}
	for _, q := range rs {
		switch q.Status {
		case leave.StatusDraft:
			d.Drafts++
		case leave.StatusPending:
			d.Pending++
		case leave.StatusApproved:
			d.Approved++
		}
		if q.Status != leave.StatusDraft {
			d.Deletable = false
		}
	}
	h.render(w, r, http.StatusOK, "admin_shutdown.html", view{Title: sd.Name, Data: d})
}

func (h *Handler) AdminGenerateDrafts(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := h.leave.GenerateDrafts(r.Context(), id); err != nil {
		if errors.Is(err, leave.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/shutdowns/%d?notice=drafts-generated", id), http.StatusSeeOther)
}

func (h *Handler) AdminDeleteShutdown(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	err := h.leave.DeleteShutdown(r.Context(), currentUser(r), id)
	switch {
	case errors.Is(err, leave.ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, leave.ErrNotAllowed):
		http.Redirect(w, r, fmt.Sprintf("/admin/shutdowns/%d?notice=not-deletable", id), http.StatusSeeOther)
		return
	case err != nil:
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/shutdowns?notice=shutdown-deleted", http.StatusSeeOther)
}
