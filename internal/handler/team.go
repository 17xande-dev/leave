package handler

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

// The month grid reaches this far from the current month: enough to see last
// year's pattern and to plan a long way ahead, small enough that a typed-in
// month cannot ask for a decade of holidays to be computed.
const (
	gridMonthsBack    = 12
	gridMonthsForward = 24
)

// teamData is the Team page: the month grid for everyone, and the approver's
// own list below it.
type teamData struct {
	Grid teamGrid
	// Reports is the leave of the people the viewer approves for, with types.
	// It is empty for anyone who approves nobody.
	Reports []leave.Request
	// HasReports is whether the viewer approves for anyone, so the list can
	// say "nobody has leave coming up" rather than vanish.
	HasReports bool
}

// teamGrid is the month grid. It carries no leave type, by construction: the
// grid is the same for an approver, an admin and a colleague, so sick leave
// stays private here (as in awayView). Approvers see types in Reports.
type teamGrid struct {
	Caption          string
	Prev, Next, This string // links; Prev/Next empty at the edge of the range, This empty on the current month
	Days             []gridDay
	Rows             []gridRow
}

type gridDay struct {
	Num     int
	Initial string // M T W ...
	Title   string // "Mon 5 Oct", plus the holiday's name
	Class   string // "off", "holiday" and/or "today"
	Holiday string
}

type gridRow struct {
	Name  string
	You   bool
	Cells []gridCell
}

type gridCell struct {
	Class string // day classes, plus "away" or "pending" on a marked cell
	Label string // visually hidden text of a marked cell
	Title string
}

// gridMonth reads ?month=: an invalid value is the current month (never
// echoed), a valid one outside the range is moved to its nearest edge.
func gridMonth(raw string, today time.Time) time.Time {
	cur := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	m, err := time.Parse("2006-01", raw)
	if err != nil {
		return cur
	}
	if lo := cur.AddDate(0, -gridMonthsBack, 0); m.Before(lo) {
		return lo
	}
	if hi := cur.AddDate(0, gridMonthsForward, 0); m.After(hi) {
		return hi
	}
	return m
}

func (h *Handler) Team(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	today := h.leave.Today()
	cur := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	month := gridMonth(r.URL.Query().Get("month"), today)
	grid, err := h.buildGrid(r, month, cur, today)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	reports, err := h.leave.Team(r.Context(), u)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	has, err := h.leave.HasReports(r.Context(), u)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "team.html", view{Title: "Team", Data: teamData{Grid: grid, Reports: reports, HasReports: has}})
}

func (h *Handler) buildGrid(r *http.Request, month, cur, today time.Time) (teamGrid, error) {
	ctx, u := r.Context(), currentUser(r)
	first, last := month, month.AddDate(0, 1, -1)
	g := teamGrid{Caption: "Who is away in " + month.Format("January 2006")}
	link := func(m time.Time) string { return "/team?month=" + m.Format("2006-01") }
	if month.After(cur.AddDate(0, -gridMonthsBack, 0)) {
		g.Prev = link(month.AddDate(0, -1, 0))
	}
	if month.Before(cur.AddDate(0, gridMonthsForward, 0)) {
		g.Next = link(month.AddDate(0, 1, 0))
	}
	if !month.Equal(cur) {
		g.This = "/team"
	}

	st, err := h.leave.Settings(ctx)
	if err != nil {
		return g, err
	}
	if err := h.leave.EnsureHolidays(ctx, month.Year()); err != nil {
		return g, err
	}
	hs, err := h.leave.HolidaysInYear(ctx, month.Year())
	if err != nil {
		return g, err
	}
	holidays := map[string]string{}
	for _, hd := range hs {
		holidays[hd.Date] = hd.Name
	}

	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		day := gridDay{Num: d.Day(), Initial: d.Format("Mon")[:1], Title: d.Format("Mon 2 Jan")}
		if name, ok := holidays[leave.FormatDate(d)]; ok {
			day.Class, day.Holiday = "holiday", name
			day.Title += ": " + name
		} else if !st.Week[d.Weekday()] {
			day.Class = "off"
		}
		if d.Equal(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)) {
			day.Class = strings.TrimSpace(day.Class + " today")
		}
		g.Days = append(g.Days, day)
	}

	// Others come from Away (one row per request); the viewer's own leave is
	// added by hand, since Away leaves the viewer out. Everyone with leave in
	// the month gets a row: there is no cap, the grid is as long as the month
	// is busy.
	type span struct{ start, end, status string }
	byUser := map[int64][]span{}
	names := map[int64]string{}
	away, err := h.leave.Away(ctx, u, leave.FormatDate(first), leave.FormatDate(last))
	if err != nil {
		return g, err
	}
	for _, a := range away {
		byUser[a.UserID] = append(byUser[a.UserID], span{a.Start, a.End, a.Status})
		names[a.UserID] = a.Name
	}
	mine, err := h.leave.ForUser(ctx, u.ID)
	if err != nil {
		return g, err
	}
	for _, rq := range mine {
		if rq.Kind != leave.KindLeave || rq.End < leave.FormatDate(first) || rq.Start > leave.FormatDate(last) {
			continue
		}
		switch rq.Status {
		case leave.StatusApproved:
			byUser[u.ID] = append(byUser[u.ID], span{rq.Start, rq.End, "approved"})
		case leave.StatusPending, leave.StatusAwaitingHR:
			byUser[u.ID] = append(byUser[u.ID], span{rq.Start, rq.End, "pending"})
		}
	}
	names[u.ID] = u.Name

	ids := make([]int64, 0, len(byUser))
	for id := range byUser {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == u.ID) != (ids[j] == u.ID) {
			return ids[i] == u.ID
		}
		if names[ids[i]] != names[ids[j]] {
			return names[ids[i]] < names[ids[j]]
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		row := gridRow{Name: names[id], You: id == u.ID}
		for _, day := range g.Days {
			date := leave.FormatDate(first.AddDate(0, 0, day.Num-1))
			// Choose the mark first: approved wins where ranges overlap, and
			// the hidden text always matches what is drawn.
			mark, label := "", ""
			for _, sp := range byUser[id] {
				if sp.start > date || sp.end < date {
					continue
				}
				if sp.status == "approved" {
					mark, label = "away", "away"
					break
				}
				mark, label = "pending", "awaiting approval"
			}
			c := gridCell{Class: strings.TrimSpace(mark + " " + day.Class), Label: label}
			c.Title = day.Holiday
			row.Cells = append(row.Cells, c)
		}
		g.Rows = append(g.Rows, row)
	}
	return g, nil
}
