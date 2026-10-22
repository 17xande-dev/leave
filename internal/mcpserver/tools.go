package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

// listCap bounds what one tool call returns: a model reads every row, and a
// list longer than this is a question to narrow, not to page through.
const listCap = 200

// --- Shapes -----------------------------------------------------------------

// RequestInfo is a request as the assistant sees it: names, not ids, wherever a
// person would use a name.
type RequestInfo struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind" jsonschema:"leave, or cancellation (a request to cancel approved leave)"`
	Person     string `json:"person"`
	Department string `json:"department,omitempty"`
	Type       string `json:"type"`
	Unit       string `json:"unit" jsonschema:"what days are counted in: workdays, calendar or weekend"`
	Start      string `json:"start" jsonschema:"first day, YYYY-MM-DD"`
	End        string `json:"end" jsonschema:"last day, YYYY-MM-DD"`
	Days       int    `json:"days"`
	Status     string `json:"status" jsonschema:"draft, awaiting_hr, pending, approved, declined, withdrawn or cancelled"`
	Note       string `json:"note,omitempty" jsonschema:"the person's note; on leave an administrator recorded (recorded_by set) it is that administrator's reason"`
	// RecordedBy is set when an administrator wrote the leave down after the fact.
	RecordedBy string `json:"recorded_by,omitempty" jsonschema:"set when an administrator recorded this leave after the fact instead of it being requested"`
	// Approver is who must decide it while it is pending; empty means any administrator.
	Approver          string `json:"approver,omitempty"`
	DecidedBy         string `json:"decided_by,omitempty"`
	DecisionNote      string `json:"decision_note,omitempty"`
	CreatedAt         string `json:"created_at"`
	CancelsRequestID  int64  `json:"cancels_request_id,omitempty"`
	Shutdown          string `json:"shutdown,omitempty" jsonschema:"set when the request was made for a company shutdown"`
	SpecialPermission bool   `json:"special_permission,omitempty" jsonschema:"over a department or team coverage limit, so every approval level must approve"`
}

func requestInfo(r leave.Request) RequestInfo {
	return RequestInfo{ID: r.ID, Kind: r.Kind, Person: r.UserName, Department: r.Department,
		Type: r.TypeName, Unit: r.TypeUnit, Start: r.Start, End: r.End, Days: r.Days, Status: r.Status,
		Note: r.Note, RecordedBy: r.RecordedBy, Approver: r.ApproverName, DecidedBy: r.DecidedBy, DecisionNote: r.DecisionNote,
		CreatedAt: r.CreatedAt, CancelsRequestID: r.CancelsID, Shutdown: r.ShutdownName,
		SpecialPermission: r.Escalate}
}

func requestInfos(rs []leave.Request) []RequestInfo {
	out := make([]RequestInfo, 0, len(rs))
	for _, r := range rs {
		out = append(out, requestInfo(r))
	}
	return out
}

// capped trims a list to listCap and says so.
func capped(rs []leave.Request) ([]leave.Request, bool) {
	if len(rs) > listCap {
		return rs[:listCap], true
	}
	return rs, false
}

type StepInfo struct {
	Approver string `json:"approver"`
	Decision string `json:"decision" jsonschema:"approved, declined, granted or refused"`
	Note     string `json:"note,omitempty"`
	At       string `json:"at"`
}

func (d *deps) steps(ctx context.Context, id int64) ([]StepInfo, error) {
	st, err := d.leave.Steps(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]StepInfo, 0, len(st))
	for _, s := range st {
		out = append(out, StepInfo{Approver: s.ApproverName, Decision: s.Decision, Note: s.Note, At: s.DecidedAt})
	}
	return out, nil
}

func plural(n int, one, many string) string {
	if n == 1 || n == -1 {
		return one
	}
	return many
}

// --- Read -------------------------------------------------------------------

type BalanceInfo struct {
	Type       string `json:"type"`
	Code       string `json:"code" jsonschema:"short code of the leave type; usable as the type for requests"`
	Unit       string `json:"unit"`
	Allowance  *int   `json:"allowance" jsonschema:"days per cycle; null means unlimited"`
	Adjustment int    `json:"adjustments" jsonschema:"days added or removed by HR (carry-over, pro-rata)"`
	Taken      int    `json:"taken" jsonschema:"approved days"`
	Pending    int    `json:"pending" jsonschema:"days awaiting approval, already reserved"`
	Available  *int   `json:"available" jsonschema:"days left to request; null means unlimited"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

type BalancesOut struct {
	Balances []BalanceInfo `json:"balances"`
}

type none struct{}

func (d *deps) myBalances(ctx context.Context, u users.User, _ none) (BalancesOut, string, error) {
	bs, err := d.leave.Balances(ctx, u.ID)
	if err != nil {
		return BalancesOut{}, "", err
	}
	out := BalancesOut{Balances: make([]BalanceInfo, 0, len(bs))}
	for _, b := range bs {
		out.Balances = append(out.Balances, BalanceInfo{Type: b.Type.Name, Code: b.Type.Code, Unit: b.Type.Unit,
			Allowance: b.Allowance, Adjustment: b.Adjustments, Taken: b.Taken, Pending: b.Pending,
			Available: b.Available(), CycleStart: b.CycleStart, CycleEnd: b.CycleEnd})
	}
	return out, fmt.Sprintf("%d leave types.", len(out.Balances)), nil
}

type MyRequestsIn struct {
	Status string `json:"status,omitempty" jsonschema:"only requests with this status: draft, awaiting_hr, pending, approved, declined, withdrawn or cancelled"`
}

type RequestsOut struct {
	Requests  []RequestInfo `json:"requests"`
	Truncated bool          `json:"truncated" jsonschema:"true when more matched than are listed"`
}

var statuses = map[string]bool{leave.StatusDraft: true, leave.StatusAwaitingHR: true, leave.StatusPending: true,
	leave.StatusApproved: true, leave.StatusDeclined: true, leave.StatusWithdrawn: true, leave.StatusCancelled: true}

const statusList = "draft, awaiting_hr, pending, approved, declined, withdrawn, cancelled"

func (d *deps) myRequests(ctx context.Context, u users.User, in MyRequestsIn) (RequestsOut, string, error) {
	if in.Status != "" && !statuses[in.Status] {
		return RequestsOut{}, "", userErrorf("Unknown status %q. Use one of: %s.", in.Status, statusList)
	}
	rs, err := d.leave.ForUser(ctx, u.ID)
	if err != nil {
		return RequestsOut{}, "", err
	}
	if in.Status != "" {
		kept := rs[:0]
		for _, r := range rs {
			if r.Status == in.Status {
				kept = append(kept, r)
			}
		}
		rs = kept
	}
	rs, trunc := capped(rs)
	return RequestsOut{Requests: requestInfos(rs), Truncated: trunc}, fmt.Sprintf("%d requests.", len(rs)), nil
}

type IDIn struct {
	ID int64 `json:"id" jsonschema:"the request's id"`
}

type RequestDetail struct {
	Request RequestInfo `json:"request"`
	Steps   []StepInfo  `json:"steps" jsonschema:"decisions so far, in order"`
	// CoverageWarnings is set for a pending request needing special permission:
	// one sentence for each department or team limit it is over.
	CoverageWarnings []string `json:"coverage_warnings,omitempty"`
	// NextApprover is who it goes to if you approve; empty when yours is the last approval.
	NextApprover        string       `json:"next_approver,omitempty"`
	ShortfallDays       int          `json:"shortfall_days,omitempty" jsonschema:"for a request awaiting HR: how far below zero it would take the balance"`
	Cancels             *RequestInfo `json:"cancels,omitempty" jsonschema:"for a cancellation: the leave it would cancel"`
	PendingCancellation int64        `json:"pending_cancellation_id,omitempty" jsonschema:"id of a cancellation waiting on this leave"`
	CanWithdraw         bool         `json:"can_withdraw"`
	CanRequestCancel    bool         `json:"can_request_cancellation"`
	CanDecide           bool         `json:"can_decide"`
	CanSubmit           bool         `json:"can_submit"`
}

// getRequest mirrors the request page's requestData, field for field.
func (d *deps) getRequest(ctx context.Context, u users.User, in IDIn) (RequestDetail, string, error) {
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestDetail{}, "", err
	}
	out := RequestDetail{
		Request:          requestInfo(req),
		CanWithdraw:      leave.CanWithdraw(u, req),
		CanRequestCancel: d.leave.CanRequestCancellation(u, req),
		CanDecide:        leave.CanDecide(u, req),
		CanSubmit:        req.UserID == u.ID && req.Status == leave.StatusDraft,
	}
	if out.Steps, err = d.steps(ctx, req.ID); err != nil {
		return out, "", err
	}
	if req.Escalate && req.Status == leave.StatusPending {
		cov, err := d.leave.CoverageFor(ctx, req)
		if err != nil {
			return out, "", err
		}
		out.CoverageWarnings = sentences(cov)
		if out.CanDecide {
			next, ok, err := d.leave.NextApprover(ctx, u, req)
			if err != nil {
				return out, "", err
			}
			if ok {
				out.NextApprover = next.Name
			}
		}
	}
	if req.Status == leave.StatusAwaitingHR {
		if out.ShortfallDays, err = d.leave.Shortfall(ctx, req); err != nil {
			return out, "", err
		}
	}
	if req.IsCancellation() {
		o, err := d.leave.Get(ctx, req.CancelsID)
		if err != nil {
			return out, "", err
		}
		info := requestInfo(o)
		out.Cancels = &info
	} else if req.Status == leave.StatusApproved {
		pc, err := d.leave.PendingCancellation(ctx, req.ID)
		if err != nil {
			return out, "", err
		}
		if pc != nil {
			out.PendingCancellation = pc.ID
			out.CanRequestCancel = false
		}
	}
	return out, fmt.Sprintf("Request %d: %s, %s to %s, %s.", req.ID, req.TypeName, req.Start, req.End, req.Status), nil
}

func (d *deps) teamLeave(ctx context.Context, u users.User, _ none) (RequestsOut, string, error) {
	rs, err := d.leave.Team(ctx, u)
	if err != nil {
		return RequestsOut{}, "", err
	}
	rs, trunc := capped(rs)
	return RequestsOut{Requests: requestInfos(rs), Truncated: trunc}, fmt.Sprintf("%d requests.", len(rs)), nil
}

type HolidaysIn struct {
	Year int `json:"year,omitempty" jsonschema:"four-digit year; default this year"`
}

type HolidayInfo struct {
	Date string `json:"date"`
	Name string `json:"name"`
}

type HolidaysOut struct {
	Year     int           `json:"year"`
	Holidays []HolidayInfo `json:"holidays"`
}

func (d *deps) publicHolidays(ctx context.Context, _ users.User, in HolidaysIn) (HolidaysOut, string, error) {
	this := d.leave.Today().Year()
	year := in.Year
	if year == 0 {
		year = this
	}
	if year < this-1 || year > this+2 {
		return HolidaysOut{}, "", userErrorf("Ask for a year between %d and %d.", this-1, this+2)
	}
	// The same lazy generation a quote does the first time a year is needed.
	if err := d.leave.EnsureHolidays(ctx, year); err != nil {
		return HolidaysOut{}, "", err
	}
	hs, err := d.leave.HolidaysInYear(ctx, year)
	if err != nil {
		return HolidaysOut{}, "", err
	}
	out := HolidaysOut{Year: year, Holidays: make([]HolidayInfo, 0, len(hs))}
	for _, h := range hs {
		out.Holidays = append(out.Holidays, HolidayInfo{Date: h.Date, Name: h.Name})
	}
	return out, fmt.Sprintf("%d public holidays in %d.", len(out.Holidays), year), nil
}

// --- Requests: preview and apply --------------------------------------------

// resolveType finds the active leave type a person means by its code or name.
func (d *deps) resolveType(ctx context.Context, name string) (leave.Type, error) {
	ts, err := d.leave.Types(ctx, true)
	if err != nil {
		return leave.Type{}, err
	}
	want := strings.TrimSpace(name)
	if want != "" {
		for _, t := range ts {
			if strings.EqualFold(t.Code, want) || strings.EqualFold(t.Name, want) {
				return t, nil
			}
		}
	}
	opts := make([]string, 0, len(ts))
	for _, t := range ts {
		opts = append(opts, fmt.Sprintf("%s (%s)", t.Code, t.Name))
	}
	return leave.Type{}, userErrorf("Leave type %q is not one of: %s.", name, strings.Join(opts, ", "))
}

type RequestIn struct {
	Type  string `json:"type" jsonschema:"the leave type's code or name, as listed by my_balances"`
	Start string `json:"start" jsonschema:"first day, YYYY-MM-DD"`
	End   string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD; default the same as start"`
	Note  string `json:"note,omitempty" jsonschema:"a short reason; some leave types require one"`
}

func (d *deps) input(ctx context.Context, in RequestIn) (leave.Input, error) {
	t, err := d.resolveType(ctx, in.Type)
	if err != nil {
		return leave.Input{}, err
	}
	end := strings.TrimSpace(in.End)
	if end == "" {
		end = strings.TrimSpace(in.Start)
	}
	return leave.Input{TypeID: t.ID, Start: strings.TrimSpace(in.Start), End: end, Note: in.Note}, nil
}

type Problem struct {
	Field   string `json:"field" jsonschema:"type, start, end, note or coverage"`
	Message string `json:"message"`
}

type PreviewOut struct {
	// OK: the request would be accepted as it is.
	OK               bool      `json:"ok"`
	Type             string    `json:"type,omitempty"`
	Start            string    `json:"start,omitempty"`
	End              string    `json:"end,omitempty"`
	Days             int       `json:"days,omitempty"`
	Unit             string    `json:"unit,omitempty"`
	AvailableBefore  *int      `json:"available_before,omitempty"`
	AvailableAfter   *int      `json:"available_after,omitempty" jsonschema:"balance left if approved; absent for unlimited types"`
	ShortfallDays    int       `json:"shortfall_days,omitempty" jsonschema:"days below zero this would take the balance"`
	CoverageWarnings []string  `json:"coverage_warnings,omitempty" jsonschema:"one for each department or team limit the request is over"`
	Problems         []Problem `json:"problems" jsonschema:"every rule the request breaks; empty when ok"`
	CanOverride      bool      `json:"can_ask_special_permission" jsonschema:"apply_for_leave with override=true would be accepted"`
	CanAskHR         bool      `json:"can_ask_hr" jsonschema:"apply_for_leave with ask_hr=true would be accepted"`
	Approver         string    `json:"approver" jsonschema:"who would decide it"`
}

// sentences turns coverage warnings into the text an assistant relays; nil
// when there are none, so the field is omitted.
func sentences(cs []leave.Coverage) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Sentence())
	}
	return out
}

// approverName is who a request of u's would go to, as the apply page says it.
func (d *deps) approverName(ctx context.Context, u users.User) (string, error) {
	id, err := d.leave.ResolveApprover(ctx, u)
	if err != nil {
		return "", err
	}
	if id == 0 {
		return "the leave administrators", nil
	}
	a, err := d.users.ByID(ctx, id)
	if err != nil {
		return "", err
	}
	return a.Name, nil
}

func (d *deps) previewRequest(ctx context.Context, u users.User, in RequestIn) (PreviewOut, string, error) {
	li, err := d.input(ctx, in)
	if err != nil {
		return PreviewOut{}, "", err
	}
	qt, err := d.leave.Preview(ctx, u, li)
	var fe leave.FieldErrors
	if err != nil && !asFieldErrors(err, &fe) {
		return PreviewOut{}, "", err
	}
	out := PreviewOut{OK: err == nil, Problems: []Problem{}}
	for _, k := range sortedKeys(fe) {
		out.Problems = append(out.Problems, Problem{Field: k, Message: fe[k]})
	}
	out.CanOverride, out.CanAskHR = leave.Offers(qt, fe)
	if qt.Days > 0 {
		out.Type, out.Unit, out.Days = qt.Type.Name, qt.Type.Unit, qt.Days
		out.Start, out.End = qt.Start.Format(time.DateOnly), qt.End.Format(time.DateOnly)
		out.AvailableBefore, out.AvailableAfter, out.ShortfallDays = qt.Balance.Available(), qt.After, qt.Shortfall
	}
	out.CoverageWarnings = sentences(qt.Coverage)
	if out.Approver, err = d.approverName(ctx, u); err != nil {
		return out, "", err
	}
	summary := "This request would be accepted."
	if !out.OK {
		summary = fmt.Sprintf("This request would be refused: %d %s.", len(out.Problems), plural(len(out.Problems), "problem", "problems"))
	}
	return out, summary, nil
}

type ApplyIn struct {
	Type     string `json:"type" jsonschema:"the leave type's code or name, as listed by my_balances"`
	Start    string `json:"start" jsonschema:"first day, YYYY-MM-DD"`
	End      string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD; default the same as start"`
	Note     string `json:"note,omitempty" jsonschema:"a short reason; some leave types require one"`
	Override bool   `json:"override,omitempty" jsonschema:"ask for special permission when over a department or team coverage limit"`
	AskHR    bool   `json:"ask_hr,omitempty" jsonschema:"ask HR to allow the balance to go below zero"`
}

type RequestOut struct {
	Request RequestInfo `json:"request"`
	Message string      `json:"message"`
}

func (d *deps) apply(ctx context.Context, u users.User, in ApplyIn) (RequestOut, string, error) {
	li, err := d.input(ctx, RequestIn{Type: in.Type, Start: in.Start, End: in.End, Note: in.Note})
	if err != nil {
		return RequestOut{}, "", err
	}
	li.Override, li.Negative = in.Override, in.AskHR
	req, err := d.leave.Submit(ctx, u, li)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Submitted for approval."
	if req.ApproverName != "" {
		msg = "Submitted for approval by " + req.ApproverName + "."
	}
	if req.Status == leave.StatusAwaitingHR {
		msg = "Sent to HR. They need to allow this before it can be submitted, and the person will be emailed."
	}
	return RequestOut{Request: requestInfo(req), Message: msg}, msg, nil
}

// --- Requests: act on one ---------------------------------------------------

func (d *deps) withdraw(ctx context.Context, u users.User, in IDIn) (RequestOut, string, error) {
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	if err := d.leave.Withdraw(ctx, u, req.ID); err != nil {
		return RequestOut{}, "", err
	}
	return d.after(ctx, req.ID, "Request withdrawn.")
}

// after reports a request as it stands once an action has run.
func (d *deps) after(ctx context.Context, id int64, msg string) (RequestOut, string, error) {
	r, err := d.leave.Get(ctx, id)
	if err != nil {
		return RequestOut{}, "", err
	}
	return RequestOut{Request: requestInfo(r), Message: msg}, msg, nil
}

type SubmitDraftIn struct {
	ID       int64  `json:"id" jsonschema:"the draft request's id"`
	Note     string `json:"note,omitempty" jsonschema:"leave blank to keep the draft's own note"`
	Override bool   `json:"override,omitempty" jsonschema:"ask for special permission when over a department or team coverage limit"`
}

func (d *deps) submitDraft(ctx context.Context, u users.User, in SubmitDraftIn) (RequestOut, string, error) {
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	out, err := d.leave.SubmitDraftOverride(ctx, u, req.ID, in.Note, in.Override)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Submitted for approval."
	if out.Status == leave.StatusApproved {
		msg = "Submitted and approved automatically."
	}
	return RequestOut{Request: requestInfo(out), Message: msg}, msg, nil
}

type CancelIn struct {
	ID   int64  `json:"id" jsonschema:"the id of the approved leave to cancel"`
	Note string `json:"note,omitempty" jsonschema:"why"`
}

func (d *deps) requestCancellation(ctx context.Context, u users.User, in CancelIn) (RequestOut, string, error) {
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	c, err := d.leave.RequestCancellation(ctx, u, req.ID, in.Note)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Cancellation sent for approval. The leave stays booked until it is approved."
	return RequestOut{Request: requestInfo(c), Message: msg}, msg, nil
}

// --- Approve ----------------------------------------------------------------

type QueueItem struct {
	Request          RequestInfo `json:"request"`
	CoverageWarnings []string    `json:"coverage_warnings,omitempty"`
	NextApprover     string      `json:"next_approver,omitempty" jsonschema:"who it goes to if you approve; empty when yours is the last approval"`
	Steps            []StepInfo  `json:"steps"`
}

type QueueOut struct {
	Queue []QueueItem `json:"queue"`
}

// approvalQueue mirrors the Approvals page, adding each row's trail.
func (d *deps) approvalQueue(ctx context.Context, u users.User, _ none) (QueueOut, string, error) {
	q, err := d.leave.Queue(ctx, u)
	if err != nil {
		return QueueOut{}, "", err
	}
	out := QueueOut{Queue: make([]QueueItem, 0, len(q))}
	for _, req := range q {
		item := QueueItem{Request: requestInfo(req)}
		if item.Steps, err = d.steps(ctx, req.ID); err != nil {
			return QueueOut{}, "", err
		}
		if req.Escalate {
			cov, err := d.leave.CoverageFor(ctx, req)
			if err != nil {
				return QueueOut{}, "", err
			}
			item.CoverageWarnings = sentences(cov)
			next, ok, err := d.leave.NextApprover(ctx, u, req)
			if err != nil {
				return QueueOut{}, "", err
			}
			if ok {
				item.NextApprover = next.Name
			}
		}
		out.Queue = append(out.Queue, item)
	}
	return out, fmt.Sprintf("%d %s waiting for you.", len(out.Queue), plural(len(out.Queue), "request", "requests")), nil
}

type DecideIn struct {
	ID       int64  `json:"id" jsonschema:"the request's id"`
	Decision string `json:"decision" jsonschema:"exactly approve or decline"`
	Note     string `json:"note,omitempty" jsonschema:"required when declining: say why"`
}

func (d *deps) decide(ctx context.Context, u users.User, in DecideIn) (RequestOut, string, error) {
	// An explicit word, never a default: anything else is refused, as the page does.
	var approve bool
	switch in.Decision {
	case "approve":
		approve = true
	case "decline":
	default:
		return RequestOut{}, "", userError(`decision must be exactly "approve" or "decline".`)
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	decided, err := d.leave.Decide(ctx, u, req.ID, approve, in.Note)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Request declined."
	if approve {
		msg = "Request approved."
		if decided.Status == leave.StatusPending {
			msg = "Approved, and sent on to the next person, because it needs special permission."
		}
	}
	return RequestOut{Request: requestInfo(decided), Message: msg}, msg, nil
}

// --- Administrators ----------------------------------------------------------

type AllRequestsIn struct {
	Status   string `json:"status,omitempty" jsonschema:"only this status: draft, awaiting_hr, pending, approved, declined, withdrawn or cancelled"`
	From     string `json:"from,omitempty" jsonschema:"only requests ending on or after this date, YYYY-MM-DD"`
	To       string `json:"to,omitempty" jsonschema:"only requests starting on or before this date, YYYY-MM-DD"`
	Username string `json:"username,omitempty" jsonschema:"only this person's requests, by sign-in username"`
}

func (d *deps) allRequests(ctx context.Context, u users.User, in AllRequestsIn) (RequestsOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestsOut{}, "", err
	}
	f := leave.Filter{Status: in.Status, From: in.From, To: in.To}
	if f.Status != "" && !statuses[f.Status] {
		return RequestsOut{}, "", userErrorf("Unknown status %q. Use one of: %s.", f.Status, statusList)
	}
	// Dates go into a string comparison in SQL, so each is parsed first.
	for _, p := range []struct{ name, v string }{{"from", f.From}, {"to", f.To}} {
		if p.v == "" {
			continue
		}
		if _, err := leave.ParseDate(p.v); err != nil {
			return RequestsOut{}, "", userErrorf("%s must be a date like 2026-10-05.", p.name)
		}
	}
	if in.Username != "" {
		who, err := d.users.ByUsername(ctx, strings.TrimSpace(in.Username))
		if err != nil {
			return RequestsOut{}, "", userErrorf("No one signs in as %q.", in.Username)
		}
		f.UserID = who.ID
	}
	rs, err := d.leave.All(ctx, f)
	if err != nil {
		return RequestsOut{}, "", err
	}
	rs, trunc := capped(rs)
	return RequestsOut{Requests: requestInfos(rs), Truncated: trunc}, fmt.Sprintf("%d requests.", len(rs)), nil
}

type AwayIn struct {
	From       string `json:"from,omitempty" jsonschema:"first day of the range, YYYY-MM-DD; default today"`
	To         string `json:"to,omitempty" jsonschema:"last day of the range, YYYY-MM-DD; default the same as from"`
	Department string `json:"department,omitempty" jsonschema:"only this department"`
}

type AwayPerson struct {
	Person string `json:"person"`
	Type   string `json:"type"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Status string `json:"status" jsonschema:"approved or pending"`
}

type AwayDepartment struct {
	Department string       `json:"department"`
	People     []AwayPerson `json:"people"`
}

type AwayOut struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	Departments []AwayDepartment `json:"departments"`
}

// maxAwayRange keeps one question to about a year.
const maxAwayRange = 366 * 24 * time.Hour

func (d *deps) whoIsAway(ctx context.Context, u users.User, in AwayIn) (AwayOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return AwayOut{}, "", err
	}
	from := d.leave.Today()
	if in.From != "" {
		t, err := leave.ParseDate(in.From)
		if err != nil {
			return AwayOut{}, "", userError("from must be a date like 2026-10-05.")
		}
		from = t
	}
	to := from
	if in.To != "" {
		t, err := leave.ParseDate(in.To)
		if err != nil {
			return AwayOut{}, "", userError("to must be a date like 2026-10-05.")
		}
		to = t
	}
	if to.Before(from) || to.Sub(from) > maxAwayRange {
		return AwayOut{}, "", userError("to must not be before from, and the range at most a year.")
	}
	f, t := from.Format(time.DateOnly), to.Format(time.DateOnly)
	// Overlap, by the same filter the admin's requests page uses; what is
	// still live is picked out here.
	rs, err := d.leave.All(ctx, leave.Filter{From: f, To: t})
	if err != nil {
		return AwayOut{}, "", err
	}
	dept := strings.TrimSpace(in.Department)
	byDept := map[string][]AwayPerson{}
	n := 0
	for _, r := range rs {
		if r.Kind != leave.KindLeave || (r.Status != leave.StatusApproved && r.Status != leave.StatusPending) {
			continue
		}
		if dept != "" && !strings.EqualFold(r.Department, dept) {
			continue
		}
		name := r.Department
		if name == "" {
			name = "(no department)"
		}
		byDept[name] = append(byDept[name], AwayPerson{Person: r.UserName, Type: r.TypeName,
			Start: r.Start, End: r.End, Status: r.Status})
		n++
	}
	out := AwayOut{From: f, To: t, Departments: []AwayDepartment{}}
	for _, k := range sortedKeys(byDept) {
		people := byDept[k]
		out.Departments = append(out.Departments, AwayDepartment{Department: k, People: people})
	}
	return out, fmt.Sprintf("%d %s away between %s and %s.", n, plural(n, "request", "requests"), f, t), nil
}

type ProblemJob struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind" jsonschema:"email, calendar_create or calendar_delete"`
	Summary   string `json:"summary" jsonschema:"what the job is for, without the message body"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt string `json:"created_at"`
	GaveUp    bool   `json:"gave_up" jsonschema:"true when retries are exhausted and it waits for an administrator to retry it"`
}

type OutboxOut struct {
	Problems []ProblemJob `json:"problems"`
}

func (d *deps) outboxProblems(ctx context.Context, u users.User, _ none) (OutboxOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return OutboxOut{}, "", err
	}
	js, err := d.outbox.Problems(ctx)
	if err != nil {
		return OutboxOut{}, "", err
	}
	out := OutboxOut{Problems: make([]ProblemJob, 0, len(js))}
	for _, j := range js {
		out.Problems = append(out.Problems, ProblemJob{ID: j.ID, Kind: j.Kind, Summary: outbox.Summary(j),
			Attempts: j.Attempts, LastError: j.LastError, CreatedAt: j.CreatedAt, GaveUp: j.FailedAt != ""})
	}
	return out, fmt.Sprintf("%d %s.", len(out.Problems), plural(len(out.Problems), "problem", "problems")), nil
}

func asFieldErrors(err error, fe *leave.FieldErrors) bool { return errors.As(err, fe) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
