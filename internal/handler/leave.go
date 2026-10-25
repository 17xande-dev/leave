package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

type homeData struct {
	Balances []leave.Balance
	// Upcoming (soonest first) and Past (newest first) split the person's own
	// requests on today; see newHomeData.
	Upcoming, Past []leave.Request
	// HasRequests is false only when there is nothing at all, drafts included.
	HasRequests bool
	// Next is the next approved leave, nil when none is booked.
	Next *nextLeave
	// Pending counts requests awaiting a decision, HR's included; PendingFirst is the soonest.
	Pending      int
	PendingFirst int64
	// Annual is what is left of annual leave; nil when the type is missing,
	// inactive or unlimited, so no number is shown rather than a wrong one.
	Annual *int
	// AnnualOver is how many days below zero annual leave is, 0 when it is not.
	AnnualOver int
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
	hd := newHomeData(bs, rs, h.leave.Today())
	hd.Drafts = drafts
	h.render(w, r, http.StatusOK, "home.html", view{Title: "My leave", Data: hd})
}

// nextLeave is an approved leave that has not finished, with where it sits
// relative to today.
type nextLeave struct {
	Request leave.Request
	// Now: it has started. Otherwise it starts In days from today.
	Now bool
	In  int
}

// newHomeData builds the home page from the person's balances and requests
// (newest start first, as Service.ForUser returns them). today comes from the
// service's clock so the page and the rules agree on the day.
func newHomeData(bs []leave.Balance, rs []leave.Request, today time.Time) homeData {
	d := homeData{Balances: bs, HasRequests: len(rs) > 0}
	for _, b := range bs {
		if b.Type.Code == leave.AnnualCode {
			d.Annual = b.Available()
			if d.Annual != nil && *d.Annual < 0 {
				d.AnnualOver = -*d.Annual
			}
		}
	}
	// YYYY-MM-DD compares as text.
	day := leave.FormatDate(today)
	// Upcoming is what is still live: anything waiting for a decision (pending
	// or awaiting HR), whatever its dates, and approved leave that has not
	// ended. A request still waiting stays up here even when its dates have
	// passed (backdating is normal), so it is not hidden under Past and still
	// counts as waiting. An approved cancellation is done, so it is Past, as
	// are declined, withdrawn and cancelled requests. Drafts are in neither
	// list: they have their own panel and are not a request yet.
	upcoming := func(r leave.Request) bool {
		switch r.Status {
		case leave.StatusPending, leave.StatusAwaitingHR:
			return true
		case leave.StatusApproved:
			return r.Kind == leave.KindLeave && r.End >= day
		}
		return false
	}
	for _, r := range rs {
		switch {
		case r.Status == leave.StatusDraft:
		case upcoming(r):
			d.Upcoming = append(d.Upcoming, r)
		default:
			d.Past = append(d.Past, r)
		}
	}
	// rs is newest first; upcoming reads better soonest first.
	slices.Reverse(d.Upcoming)
	for _, r := range d.Upcoming {
		if r.Status == leave.StatusPending || r.Status == leave.StatusAwaitingHR {
			d.Pending++
			if d.PendingFirst == 0 {
				d.PendingFirst = r.ID
			}
		}
		if d.Next == nil && r.Status == leave.StatusApproved && r.Kind == leave.KindLeave {
			n := &nextLeave{Request: r, Now: r.Start <= day}
			if start, err := leave.ParseDate(r.Start); err == nil && !n.Now {
				n.In = leave.DaysBetween(today, start)
			}
			d.Next = n
		}
	}
	return d
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
	// Labels maps a type's ID to its option text: the name, with what is left
	// of the balance when the type has one.
	Labels map[int64]string
	// Marks is the picker's advisory marks: holidays and the person's own leave.
	Marks pickerMarks
}

// typeLabel is the select's option text: "Annual leave (11 left)". Unlimited
// types show just the name; an overdrawn balance reads as 0, since the person
// cannot request any more either way.
func typeLabel(b leave.Balance) string {
	avail := b.Available()
	if avail == nil {
		return b.Type.Name
	}
	n := max(*avail, 0)
	// The balance is for the cycle containing today, which the quote names
	// when the dates fall in another. Days are the usual count; only weekends
	// need naming.
	if b.Type.Unit == leave.UnitWeekend {
		return fmt.Sprintf("%s (%d %s left)", b.Type.Name, n, unitLabel(n, b.Type.Unit))
	}
	if b.Type.CycleMonths == 12 {
		return fmt.Sprintf("%s (%d left this year)", b.Type.Name, n)
	}
	return fmt.Sprintf("%s (%d left this cycle)", b.Type.Name, n)
}

// pickerMarks (given the person's own requests) is the JSON data block behind the date picker's marks: public
// holidays by date, and the person's live leave as ranges. It is advisory
// only (the server still validates), so it covers a window rather than all
// time: 92 days back, for recording leave just taken or checking a recent
// booking, to a year ahead, the furthest anyone plans.
type pickerMarks struct {
	Holidays map[string]string `json:"holidays"`
	Booked   []markedRange     `json:"booked"`
}

type markedRange struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Status string `json:"status"`
}

func (h *Handler) pickerMarks(ctx context.Context, rs []leave.Request) (pickerMarks, error) {
	today := h.leave.Today()
	from, to := leave.FormatDate(today.AddDate(0, 0, -92)), leave.FormatDate(today.AddDate(1, 0, 0))
	m := pickerMarks{Holidays: map[string]string{}, Booked: []markedRange{}}
	for y := today.AddDate(0, 0, -92).Year(); y <= today.AddDate(1, 0, 0).Year(); y++ {
		if err := h.leave.EnsureHolidays(ctx, y); err != nil {
			return m, err
		}
		hs, err := h.leave.HolidaysInYear(ctx, y)
		if err != nil {
			return m, err
		}
		for _, hd := range hs {
			if hd.Date >= from && hd.Date <= to {
				m.Holidays[hd.Date] = hd.Name
			}
		}
	}
	for _, r := range rs {
		// Drafts are not booked, and a cancellation request is not itself leave.
		if r.Kind != leave.KindLeave || r.End < from || r.Start > to {
			continue
		}
		switch r.Status {
		case leave.StatusApproved:
			m.Booked = append(m.Booked, markedRange{r.Start, r.End, "approved"})
		case leave.StatusPending, leave.StatusAwaitingHR:
			m.Booked = append(m.Booked, markedRange{r.Start, r.End, "pending"})
		}
	}
	return m, nil
}

func (h *Handler) applyData(r *http.Request, in leave.Input, errs leave.FieldErrors) (applyData, error) {
	// One balances lookup serves both the type list (active types, in display
	// order) and the "n left" in each option.
	bs, err := h.leave.Balances(r.Context(), currentUser(r).ID)
	if err != nil {
		return applyData{}, err
	}
	types := make([]leave.Type, len(bs))
	d := applyData{Input: in, Errors: errs, Preview: quoteData{Incomplete: true}, Mins: map[int64]string{}, Labels: map[int64]string{}}
	for i, b := range bs {
		types[i] = b.Type
		d.Labels[b.Type.ID] = typeLabel(b)
	}
	d.Types = types
	mine, err := h.leave.ForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		return applyData{}, err
	}
	if d.Marks, err = h.pickerMarks(r.Context(), mine); err != nil {
		return applyData{}, err
	}
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
			if d.Preview, err = h.withAway(r, newQuoteData(&qt, fe), in.Start, in.End); err != nil {
				return applyData{}, err
			}
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
	// Away is who else is away in the chosen dates (at most awayShown of
	// them), and AwayMore how many more there are. It has no leave type by
	// construction: this hint is the same for an approver and a colleague.
	Away     []awayView
	AwayMore int
}

// awayShown caps the names in the "Also away then" hint.
const awayShown = 5

// awayView is one person in the hint: name and dates, nothing private.
type awayView struct {
	Name    string
	Start   string
	End     string
	Pending bool
}

// withAway adds who else is away between start and end, for the chosen dates.
// It is information only: unusable dates just leave the hint out.
func (h *Handler) withAway(r *http.Request, d quoteData, start, end string) (quoteData, error) {
	s, err1 := leave.ParseDate(start)
	e, err2 := leave.ParseDate(end)
	if err1 != nil || err2 != nil || e.Before(s) {
		return d, nil
	}
	away, err := h.leave.Away(r.Context(), currentUser(r), start, end)
	if err != nil {
		return d, err
	}
	for i, a := range away {
		if i == awayShown {
			d.AwayMore = len(away) - awayShown
			break
		}
		d.Away = append(d.Away, awayView{Name: a.Name, Start: a.Start, End: a.End, Pending: a.Status == "pending"})
	}
	return d, nil
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
	d, err := h.withAway(r, newQuoteData(&qt, fe), in.Start, in.End)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.renderPartial(w, r, "quote", d)
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
	http.Redirect(w, r, requestURL(r, req.ID, notice), http.StatusSeeOther)
}

type requestData struct {
	Request leave.Request
	// Back is where the page's back link goes, from where the person came, and
	// From is the key of backLinks it came from, empty for the default. The forms
	// pass From on so the link survives the next step.
	Back backLink
	From string
	// Original is the leave a cancellation request is for.
	Original *leave.Request
	// PendingCancellation is a cancellation waiting on this leave.
	PendingCancellation *leave.Request
	CanWithdraw         bool
	CanCancel           bool
	CanDecide           bool
	// CanUnrecord: an admin may remove this recorded leave.
	CanUnrecord bool
	CanSubmit   bool
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
		Back:        backLink{"/", "My leave"},
		CanWithdraw: leave.CanWithdraw(u, req),
		CanCancel:   h.leave.CanRequestCancellation(u, req),
		CanDecide:   leave.CanDecide(u, req),
		CanUnrecord: u.Admin && req.IsRecorded() && req.Status == leave.StatusApproved && req.UserID != u.ID,
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
	if k := fromKey(r); k != "" {
		d.Back, d.From = backLinks[k], k
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

type backLink struct{ Href, Label string }

// backLinks maps the ?from= a list page adds to its links onto a back link. The
// map is fixed so the query text is never echoed: an unknown value is the default.
var backLinks = map[string]backLink{
	"approvals": {"/approvals", "Approvals"},
	"team":      {"/team", "Team"},
	"admin":     {"/admin/requests", "Admin"},
}

// fromKey is the ?from= of the request's URL if it is a key of backLinks, else
// empty. It is read from the URL, not the form body, which has its own "from".
func fromKey(r *http.Request) string {
	k := r.URL.Query().Get("from")
	if _, ok := backLinks[k]; ok {
		return k
	}
	return ""
}

// requestURL is where to send someone after acting on a request, keeping the
// back link they had. The key is from the fixed map, so nothing typed is echoed.
func requestURL(r *http.Request, id int64, notice string) string {
	u := fmt.Sprintf("/leave/%d?notice=%s", id, notice)
	if k := fromKey(r); k != "" {
		u += "&from=" + k
	}
	return u
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
	http.Redirect(w, r, requestURL(r, id, "not-possible"), http.StatusSeeOther)
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
	http.Redirect(w, r, requestURL(r, req.ID, "withdrawn"), http.StatusSeeOther)
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
	http.Redirect(w, r, requestURL(r, c.ID, "cancel-requested"), http.StatusSeeOther)
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
	http.Redirect(w, r, requestURL(r, out.ID, notice), http.StatusSeeOther)
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
	http.Redirect(w, r, requestURL(r, req.ID, notice), http.StatusSeeOther)
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
