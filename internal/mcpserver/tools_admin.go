package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// The administrators' action tools. Each wraps the leave.Service method the
// admin pages call, after the same admin check the read-only admin tools make;
// the service enforces the rest (no self-actions, status guards, validation).

const personDoc = "the person's sign-in username (as in all_requests), their email address, or their exact full name"

// resolvePerson finds one person by sign-in username, email address or full
// name. Every field is compared whole (case-insensitively), and the candidates
// from all three are pooled: if they are not all the same person the question
// is refused, never guessed at. The username is deliberately not normalised the
// way a sign-in is (users.ByUsername strips "@domain" and "DOMAIN\"), which
// would make "staff2@elsewhere.org" mean staff2 even when it is someone
// else's exact email; anything with an "@" or a backslash is not a username.
// Disabled people are found too: an administrator may need to correct a
// leaver's records.
func (d *deps) resolvePerson(ctx context.Context, who string) (users.User, error) {
	want := strings.TrimSpace(who)
	if want == "" {
		return users.User{}, userError("Say who: a username, email address or full name.")
	}
	all, err := d.users.All(ctx)
	if err != nil {
		return users.User{}, err
	}
	mayBeUsername := !strings.ContainsAny(want, `@\`)
	var hits []users.User
	for _, u := range all {
		if (mayBeUsername && strings.EqualFold(u.Username, want)) ||
			(u.Email != "" && strings.EqualFold(u.Email, want)) || strings.EqualFold(u.Name, want) {
			hits = append(hits, u)
		}
	}
	switch len(hits) {
	case 0:
		return users.User{}, userErrorf("No one matches %q. Use a sign-in username, email address or full name.", who)
	case 1:
		return hits[0], nil
	}
	names := make([]string, 0, len(hits))
	for _, u := range hits {
		n := fmt.Sprintf("%s (%s)", u.Name, u.Username)
		if u.Disabled {
			n = fmt.Sprintf("%s (%s, disabled)", u.Name, u.Username)
		}
		names = append(names, n)
	}
	return users.User{}, userErrorf("%q matches more than one person: %s. Use a username.", who, strings.Join(names, ", "))
}

type PersonInfo struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	Disabled bool   `json:"disabled,omitempty" jsonschema:"true when the person can no longer sign in"`
}

func personInfo(u users.User) PersonInfo {
	return PersonInfo{Name: u.Name, Username: u.Username, Email: u.Email, Disabled: u.Disabled}
}

// --- Recorded leave ---------------------------------------------------------

type SetHalfDaysIn struct {
	Seen      string `json:"seen,omitempty" jsonschema:"request fingerprint from get_request; refuses changes if the request changed since it was read"`
	ID        int64  `json:"id" jsonschema:"the leave request id"`
	HalfStart bool   `json:"half_start" jsonschema:"whether the first or only day is a half day"`
	HalfEnd   bool   `json:"half_end" jsonschema:"whether the last day is a half day; false for a one-day request"`
}

func (d *deps) setHalfDays(ctx context.Context, u users.User, in SetHalfDaysIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.leave.SetHalves(ctx, u, in.ID, in.HalfStart, in.HalfEnd, in.Seen)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Half days saved. The person has been emailed, and the approver too when the request is pending."
	return RequestOut{Request: requestInfo(req), Message: msg}, msg, nil
}

type CorrectRequestIn struct {
	ID        int64       `json:"id" jsonschema:"the request's id; it must be someone else's, not yet approved"`
	Type      string      `json:"type" jsonschema:"the leave type's code or name for the corrected request; any type offered to the person, including ones staff cannot apply for"`
	Start     string      `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD; use this or ranges"`
	End       string      `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD; default the same as start"`
	Ranges    []DateRange `json:"ranges,omitempty" jsonschema:"inclusive stretches instead of start/end; workdays types only, max 3"`
	Note      string      `json:"note,omitempty" jsonschema:"the request's note as it should read afterwards; some leave types require one"`
	HalfStart bool        `json:"half_start,omitempty" jsonschema:"the first day (or the only day) is a half day, charged 0.5; admins can always use half days"`
	HalfEnd   bool        `json:"half_end,omitempty" jsonschema:"the last day is a half day, charged 0.5; not for a one-day request"`
	Reason    string      `json:"reason" jsonschema:"why you are changing it; required, kept in the request's history and sent to the person"`
	Seen      string      `json:"seen,omitempty" jsonschema:"request fingerprint from get_request; refuses the change if the request changed since it was read"`
}

func (d *deps) correctRequest(ctx context.Context, u users.User, in CorrectRequestIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	owner, err := d.users.ByID(ctx, req.UserID)
	if err != nil {
		return RequestOut{}, "", err
	}
	t, err := d.resolveTypeIn(ctx, in.Type, false, owner)
	if err != nil {
		return RequestOut{}, "", err
	}
	li, err := requestDates(in.Start, in.End, in.Ranges)
	if err != nil {
		return RequestOut{}, "", err
	}
	li.TypeID, li.Note = t.ID, in.Note
	li.HalfStart, li.HalfEnd = in.HalfStart, in.HalfEnd
	out, err := d.leave.Correct(ctx, u, in.ID, li, in.Reason, in.Seen)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Request corrected. " + out.UserName + " has been emailed"
	if out.Status == leave.StatusPending {
		msg += ", and so has whoever approves it"
	}
	msg += "."
	return RequestOut{Request: requestInfo(out), Message: msg}, msg, nil
}

type RecordLeaveIn struct {
	Person    string      `json:"person" jsonschema:"who took the leave: the person's sign-in username, email address or exact full name"`
	Type      string      `json:"type" jsonschema:"the leave type's code or name; a retired type is allowed"`
	Start     string      `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD; use this or ranges"`
	End       string      `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD, today or earlier: only leave that has already happened can be recorded; default the same as start"`
	Ranges    []DateRange `json:"ranges,omitempty" jsonschema:"inclusive stretches instead of start/end; workdays types only, max 3; recording is exempt from the applicants' 31-day limit"`
	Note      string      `json:"note" jsonschema:"why it is being recorded; required"`
	HalfStart bool        `json:"half_start,omitempty" jsonschema:"the first day (or the only day) is a half day, charged 0.5; only for leave counted in working days, on a day that is charged; admins recording leave can always use half days"`
	HalfEnd   bool        `json:"half_end,omitempty" jsonschema:"the last day is a half day, charged 0.5; not for a one-day request (use half_start) and only for leave counted in working days; admins recording leave can always use half days"`
}

type RecordedOut struct {
	Request   RequestInfo  `json:"request"`
	Balance   *BalanceInfo `json:"balance,omitempty" jsonschema:"the person's balance for this type in the leave's cycle, after recording"`
	BelowZero bool         `json:"below_zero" jsonschema:"true when recording took the balance below zero"`
	Message   string       `json:"message"`
}

func (d *deps) recordLeave(ctx context.Context, u users.User, in RecordLeaveIn) (RecordedOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RecordedOut{}, "", err
	}
	who, err := d.resolvePerson(ctx, in.Person)
	if err != nil {
		return RecordedOut{}, "", err
	}
	t, err := d.resolveTypeIn(ctx, in.Type, false, who)
	if err != nil {
		return RecordedOut{}, "", err
	}
	li, err := requestDates(in.Start, in.End, in.Ranges)
	if err != nil {
		return RecordedOut{}, "", err
	}
	li.TypeID, li.Note = t.ID, in.Note
	li.HalfStart, li.HalfEnd = in.HalfStart, in.HalfEnd
	req, err := d.leave.RecordLeave(ctx, u, who.ID, li)
	if err != nil {
		return RecordedOut{}, "", err
	}
	out := RecordedOut{Request: requestInfo(req), BelowZero: req.AllowNegative}
	// The leave is recorded and the person told. A balance that cannot be read
	// afterwards is left out of the answer rather than reported as a failure.
	if bi, err := d.balanceFor(ctx, who.ID, t, req.CycleStart); err != nil {
		slog.Error("mcp tool: balance after recording", "tool", "record_leave", "request", req.ID, "err", err)
	} else {
		out.Balance = &bi
	}
	out.Message = fmt.Sprintf("Recorded %s %s of %s for %s. They have been emailed and it is on the shared calendar.",
		req.Days, req.Days.Plural("day", "days"), req.TypeName, req.UserName)
	if out.BelowZero {
		out.Message += " Their balance is now below zero."
	}
	return out, out.Message, nil
}

// balanceFor is a person's balance for a type in the cycle starting on cycle.
func (d *deps) balanceFor(ctx context.Context, userID int64, t leave.Type, cycle string) (BalanceInfo, error) {
	c, err := leave.ParseDate(cycle)
	if err != nil {
		return BalanceInfo{}, err
	}
	b, err := d.leave.BalanceIn(ctx, userID, t, c)
	if err != nil {
		return BalanceInfo{}, err
	}
	return balanceInfo(b), nil
}

func (d *deps) removeRecorded(ctx context.Context, u users.User, in IDIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	if !req.IsRecorded() {
		return RequestOut{}, "", userError("Only leave an administrator recorded can be removed this way; ordinary leave is cancelled through approval.")
	}
	if _, err := d.leave.Unrecord(ctx, u, req.ID); err != nil {
		return RequestOut{}, "", err
	}
	return d.after(ctx, req.ID, "Recorded leave removed. It no longer counts against their balance, and the person has been emailed.")
}

// --- Balances ---------------------------------------------------------------

type PersonBalancesIn struct {
	Person string `json:"person" jsonschema:"the person's sign-in username, email address or exact full name"`
}

type AdjustmentInfo struct {
	Type       string     `json:"type"`
	CycleStart string     `json:"cycle_start"`
	Days       leave.Days `json:"days" jsonschema:"signed: negative takes days away"`
	Reason     string     `json:"reason"`
	By         string     `json:"by"`
	CreatedAt  string     `json:"created_at"`
}

type PersonBalancesOut struct {
	Person      PersonInfo       `json:"person"`
	Balances    []BalanceInfo    `json:"balances"`
	Adjustments []AdjustmentInfo `json:"adjustments" jsonschema:"every adjustment ever made for this person, newest first"`
}

func adjustmentInfos(as []leave.Adjustment) []AdjustmentInfo {
	out := make([]AdjustmentInfo, 0, len(as))
	for _, a := range as {
		out = append(out, AdjustmentInfo{Type: a.TypeName, CycleStart: a.CycleStart, Days: a.Days,
			Reason: a.Reason, By: a.ByName, CreatedAt: a.CreatedAt})
	}
	return out
}

func (d *deps) personBalances(ctx context.Context, u users.User, in PersonBalancesIn) (PersonBalancesOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return PersonBalancesOut{}, "", err
	}
	who, err := d.resolvePerson(ctx, in.Person)
	if err != nil {
		return PersonBalancesOut{}, "", err
	}
	bs, err := d.leave.Balances(ctx, who.ID)
	if err != nil {
		return PersonBalancesOut{}, "", err
	}
	as, err := d.leave.Adjustments(ctx, who.ID)
	if err != nil {
		return PersonBalancesOut{}, "", err
	}
	out := PersonBalancesOut{Person: personInfo(who), Balances: make([]BalanceInfo, 0, len(bs)), Adjustments: adjustmentInfos(as)}
	for _, b := range bs {
		out.Balances = append(out.Balances, balanceInfo(b))
	}
	return out, fmt.Sprintf("%s: %d leave types, %d %s.", who.Name, len(out.Balances), len(out.Adjustments),
		plural(len(out.Adjustments), "adjustment", "adjustments")), nil
}

type AdjustIn struct {
	Person     string     `json:"person" jsonschema:"the person's sign-in username, email address or exact full name"`
	Type       string     `json:"type" jsonschema:"the leave type's code or name, as listed by person_balances"`
	CycleStart string     `json:"cycle_start,omitempty" jsonschema:"first day of the cycle to adjust, YYYY-MM-DD, as person_balances shows; default the current cycle"`
	Days       leave.Days `json:"days" jsonschema:"signed number of days to add (carry-over, pro-rata) or, negative, to remove; may be fractional with at most two decimals (1.5, -1.44); not zero"`
	Reason     string     `json:"reason" jsonschema:"why; required, kept on the person's record"`
}

type AdjustOut struct {
	Person      PersonInfo       `json:"person"`
	Balance     *BalanceInfo     `json:"balance,omitempty" jsonschema:"the balance for that type and cycle after the adjustment"`
	Adjustments []AdjustmentInfo `json:"adjustments"`
	Message     string           `json:"message"`
}

func (d *deps) adjustBalance(ctx context.Context, u users.User, in AdjustIn) (AdjustOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return AdjustOut{}, "", err
	}
	who, err := d.resolvePerson(ctx, in.Person)
	if err != nil {
		return AdjustOut{}, "", err
	}
	t, err := d.resolveType(ctx, in.Type, who)
	if err != nil {
		return AdjustOut{}, "", err
	}
	cycle := strings.TrimSpace(in.CycleStart)
	if cycle == "" {
		// The cycle that holds today is the one the person's balances show.
		bs, err := d.leave.Balances(ctx, who.ID)
		if err != nil {
			return AdjustOut{}, "", err
		}
		for _, b := range bs {
			if b.Type.ID == t.ID {
				cycle = b.CycleStart
			}
		}
	}
	a := leave.Adjustment{UserID: who.ID, TypeID: t.ID, CycleStart: cycle, Days: in.Days, Reason: in.Reason}
	if err := d.leave.AddAdjustment(ctx, u, a); err != nil {
		return AdjustOut{}, "", err
	}
	out := AdjustOut{Person: personInfo(who)}
	if bi, err := d.balanceFor(ctx, who.ID, t, cycle); err != nil {
		slog.Error("mcp tool: balance after adjusting", "tool", "adjust_balance", "err", err)
	} else {
		out.Balance = &bi
	}
	as, err := d.leave.Adjustments(ctx, who.ID)
	if err != nil {
		return out, "", err
	}
	out.Adjustments = adjustmentInfos(as)
	out.Message = fmt.Sprintf("Adjusted %s's %s balance for the cycle from %s by %s %s. Nobody is emailed.",
		who.Name, t.Name, cycle, signedDays(in.Days), in.Days.Plural("day", "days"))
	return out, out.Message, nil
}

// --- Permission to go below zero -------------------------------------------

type PermissionItem struct {
	Request       RequestInfo `json:"request"`
	ShortfallDays leave.Days  `json:"shortfall_days" jsonschema:"how far below zero it would take the balance, as it stands now"`
}

type PermissionsOut struct {
	Queue []PermissionItem `json:"queue"`
}

func (d *deps) permissionQueue(ctx context.Context, u users.User, _ none) (PermissionsOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return PermissionsOut{}, "", err
	}
	rs, err := d.leave.AwaitingHR(ctx)
	if err != nil {
		return PermissionsOut{}, "", err
	}
	out := PermissionsOut{Queue: make([]PermissionItem, 0, len(rs))}
	for _, r := range rs {
		sf, err := d.leave.Shortfall(ctx, r)
		if err != nil {
			return PermissionsOut{}, "", err
		}
		out.Queue = append(out.Queue, PermissionItem{Request: requestInfo(r), ShortfallDays: sf})
	}
	return out, fmt.Sprintf("%d %s waiting for HR.", len(out.Queue), plural(len(out.Queue), "request", "requests")), nil
}

type NegativeIn struct {
	ID   int64  `json:"id" jsonschema:"the id of a request awaiting_hr, from permission_queue"`
	Note string `json:"note,omitempty" jsonschema:"optional; the person sees it"`
}

type RefuseIn struct {
	ID   int64  `json:"id" jsonschema:"the id of a request awaiting_hr, from permission_queue"`
	Note string `json:"note" jsonschema:"required: say why, so the person knows what to change; they see it"`
}

func (d *deps) grantNegative(ctx context.Context, u users.User, in NegativeIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	if _, err := d.leave.GrantNegative(ctx, u, req.ID, in.Note); err != nil {
		return RequestOut{}, "", err
	}
	return d.after(ctx, req.ID, "Permission granted. The request is a draft again for the person to submit, and they have been emailed.")
}

func (d *deps) refuseNegative(ctx context.Context, u users.User, in RefuseIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	if _, err := d.leave.RefuseNegative(ctx, u, req.ID, in.Note); err != nil {
		return RequestOut{}, "", err
	}
	return d.after(ctx, req.ID, "Permission refused. The request is declined and the person has been emailed.")
}

// --- Reassign ---------------------------------------------------------------

type ReassignIn struct {
	ID       int64  `json:"id" jsonschema:"the id of a pending request"`
	Approver string `json:"approver,omitempty" jsonschema:"the new approver: sign-in username, email address or exact full name; leave blank to hand it to the leave administrators"`
}

func (d *deps) reassign(ctx context.Context, u users.User, in ReassignIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	var approverID int64
	if strings.TrimSpace(in.Approver) != "" {
		a, err := d.resolvePerson(ctx, in.Approver)
		if err != nil {
			return RequestOut{}, "", err
		}
		approverID = a.ID
	}
	moved, err := d.leave.Reassign(ctx, u, req.ID, approverID)
	if err != nil {
		return RequestOut{}, "", err
	}
	msg := "Request handed to the leave administrators, who have been emailed."
	if moved.ApproverName != "" {
		msg = "Request reassigned to " + moved.ApproverName + ", who has been emailed."
	}
	return RequestOut{Request: requestInfo(moved), Message: msg}, msg, nil
}

func (d *deps) registerAdminActions(s *mcp.Server) {
	add(d, s, &mcp.Tool{Name: "set_half_days", Annotations: destructive("Change half days (administrators)"),
		Description: "Administrators only: set or clear half days on another person's live working-day leave, including approved leave. Works even when applicant half days are off. Refuses shutdown leave and leave with a pending cancellation. The days and balance are recomputed, the change is audited, and the person (and approver when pending) is emailed. Only when the person has told you to."}, d.setHalfDays)
	add(d, s, &mcp.Tool{Name: "correct_request", Annotations: destructive("Correct a request (administrators)"),
		Description: "Administrators only: change the type, dates, half days and note of another person's request that has not been approved yet (pending, waiting for HR, or an HR-allowed draft) to fix an incorrect submission, for example annual leave that should be compassionate or unpaid. " +
			"State the whole request as it should read (type, start/end or ranges, half days, note), not only what changes; omitting the note or the half-day flags clears them. It works on types staff cannot apply for, ignores coverage limits and the balance (a shortfall is allowed), keeps the approver and the request's place in the queue, and the days are recounted. " +
			"A reason is required; it is kept in the request's history and the person (and the approver while it is pending) is emailed. Refused for your own request (another administrator corrects it), for approved, declined, recorded or shutdown leave, " +
			"and for a request someone has started approving. Pass seen (the fingerprint from get_request) to refuse the change if the request changed since you read it. Only when the person has told you to."}, d.correctRequest)
	add(d, s, &mcp.Tool{Name: "person_balances", Annotations: readOnly("A person's balances (administrators)"),
		Description: "Administrators only: one person's balances for the current cycle, with the cycle start dates, and every balance adjustment made for them."}, d.personBalances)
	add(d, s, &mcp.Tool{Name: "permission_queue", Annotations: readOnly("Requests waiting for HR (administrators)"),
		Description: "Administrators only: requests waiting for HR to allow the balance to go below zero (awaiting_hr), oldest first, with how far below zero each would go."}, d.permissionQueue)

	add(d, s, &mcp.Tool{Name: "record_leave", Annotations: additive("Record leave a person took (administrators)"),
		Description: "Administrators only: write down leave a person has already started or taken, stored as approved. The person is emailed and it goes on the shared calendar. " +
			"A note saying why is required. Leave whose last day is still to come is refused (future days go through an application); any type, including ones staff cannot apply for or a retired type, is allowed; the balance may go below zero (reported as below_zero). " +
			"You cannot record your own leave; another administrator does that. Only when the person has told you to. " +
			"Use start/end for one stretch or ranges: [{start, end}] instead, never both. Several stretches are for workdays types only, max 3; recording is exempt from the applicants' 31-day limit. " +
			"half_start / half_end apply to the first day of the first range and last day of the last range of the whole request (0.5); admins recording leave can always use half days."}, d.recordLeave)
	add(d, s, &mcp.Tool{Name: "remove_recorded_leave", Annotations: destructive("Remove recorded leave (administrators)"),
		Description: "Administrators only: remove leave an administrator recorded (not ordinary approved leave). It no longer counts against their balance, the calendar event goes and the person is emailed. Cannot be undone. You cannot remove leave recorded for you. Only when the person has told you to."}, d.removeRecorded)
	add(d, s, &mcp.Tool{Name: "adjust_balance", Annotations: additive("Adjust a balance (administrators)"),
		Description: "Administrators only: add or remove days on a person's balance for one leave type and cycle (carry-over, pro-rata, correction). A reason is required. " +
			"The cycle defaults to the current one. Nobody is emailed. You cannot adjust your own balance. An adjustment cannot be edited or deleted afterwards, only offset with another. Only when the person has told you to."}, d.adjustBalance)
	add(d, s, &mcp.Tool{Name: "grant_negative", Annotations: destructive("Allow a negative balance (administrators)"),
		Description: "Administrators only: allow a request waiting for HR to take the balance below zero. It returns to the person as a draft for them to submit, and they are emailed with your note. " +
			"You cannot grant your own request. Only when the person has told you to."}, d.grantNegative)
	add(d, s, &mcp.Tool{Name: "refuse_negative", Annotations: destructive("Refuse a negative balance (administrators)"),
		Description: "Administrators only: refuse a request waiting for HR. It ends as declined, the person is emailed your note, and it cannot be undone. A note saying why is required. " +
			"You cannot refuse your own request. Only when the person has told you to."}, d.refuseNegative)
	add(d, s, &mcp.Tool{Name: "reassign_request", Annotations: destructive("Reassign a request (administrators)"),
		Description: "Administrators only: move a pending request. The new approver must be an administrator other than the person whose request it is; blank hands it to the leave administrators. You may reassign your own request to another administrator. The new approver is emailed. Only for requests still pending. Only when the person has told you to."}, d.reassign)
}

// signedDays is a days figure with its sign always shown: "+1.5", "-2".
func signedDays(d leave.Days) string {
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}
