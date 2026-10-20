package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/17xande-dev/leave/internal/leave"
)

// --- Teams ---------------------------------------------------------------

// teamRow is one line of the Teams page. Percent is as typed, so a rejected
// entry comes back beside its team.
type teamRow struct {
	leave.Team
	Percent string
	Error   string
}

type teamsData struct {
	// Enabled is whether a team source is configured at all.
	Enabled bool
	Default teamRow
	Rows    []teamRow
	// Inactive: teams gone from the source are listed too.
	Inactive bool
}

// teamsPage renders the page; failed, if set, is a rejected entry (ID 0 for
// the default) to show again with its error.
func (h *Handler) teamsPage(w http.ResponseWriter, r *http.Request, status int, inactive bool, failed *teamRow) {
	d := teamsData{Enabled: h.teamsOn, Inactive: inactive}
	if h.teamsOn {
		st, err := h.leave.Settings(r.Context())
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		if st.TeamDefaultPercent > 0 {
			d.Default.Percent = strconv.Itoa(st.TeamDefaultPercent)
		}
		teams, err := h.leave.Teams(r.Context(), inactive)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		for _, t := range teams {
			row := teamRow{Team: t}
			if t.MaxPercent > 0 {
				row.Percent = strconv.Itoa(t.MaxPercent)
			}
			d.Rows = append(d.Rows, row)
		}
		if failed != nil {
			if failed.ID == 0 {
				d.Default = *failed
			}
			for i := range d.Rows {
				if d.Rows[i].ID == failed.ID {
					d.Rows[i].Percent, d.Rows[i].Error = failed.Percent, failed.Error
				}
			}
		}
	}
	h.render(w, r, status, "admin_teams.html", view{Title: "Teams", Data: d})
}

func (h *Handler) AdminTeams(w http.ResponseWriter, r *http.Request) {
	h.teamsPage(w, r, http.StatusOK, r.URL.Query().Get("inactive") == "1", nil)
}

const teamPercentError = "Enter a whole number from 1 to 100, or leave it empty for no limit."

func (h *Handler) AdminSaveTeam(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("team"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "Unknown team.", http.StatusBadRequest)
		return
	}
	raw := r.PostFormValue("percent")
	n, ok := parsePercent(raw)
	if !ok {
		// An inactive team is the only one whose row needs the inactive list.
		h.teamsPage(w, r, http.StatusUnprocessableEntity, true, &teamRow{Team: leave.Team{ID: id}, Percent: raw, Error: teamPercentError})
		return
	}
	switch err := h.leave.SetTeamRule(r.Context(), id, n); {
	case errors.Is(err, leave.ErrNotFound):
		http.Error(w, "Unknown team.", http.StatusBadRequest)
	case err != nil:
		h.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/admin/teams?notice=team-saved", http.StatusSeeOther)
	}
}

func (h *Handler) AdminSaveTeamDefault(w http.ResponseWriter, r *http.Request) {
	raw := r.PostFormValue("percent")
	n, ok := parsePercent(raw)
	if !ok {
		h.teamsPage(w, r, http.StatusUnprocessableEntity, false, &teamRow{Percent: raw,
			Error: "Enter a whole number from 1 to 100, or leave it empty for no default."})
		return
	}
	if err := h.leave.SetTeamDefault(r.Context(), currentUser(r), n); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/teams?notice=team-default-saved", http.StatusSeeOther)
}
