package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// The administrators' action tools. Each wraps the leave.Service method the
// admin pages call, after the same admin check the read-only admin tools make;
// the service enforces the rest (no self-actions, status guards, validation).

const personDoc = "the person's sign-in username (as in all_requests), their email address, or their exact full name"

// resolvePerson finds one person by username, then email, then exact name.
// Disabled people are found too: an administrator may need to correct a
// leaver's records. Anything that matches more than one row is refused rather
// than guessed at.
func (d *deps) resolvePerson(ctx context.Context, who string) (users.User, error) {
	want := strings.TrimSpace(who)
	if want == "" {
		return users.User{}, userError("Say who: a username, email address or full name.")
	}
	if u, err := d.users.ByUsername(ctx, want); err == nil {
		return u, nil
	}
	all, err := d.users.All(ctx)
	if err != nil {
		return users.User{}, err
	}
	for _, field := range []func(users.User) string{
		func(u users.User) string { return u.Email },
		func(u users.User) string { return u.Name },
	} {
		var hits []users.User
		for _, u := range all {
			if v := field(u); v != "" && strings.EqualFold(v, want) {
				hits = append(hits, u)
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			names := make([]string, 0, len(hits))
			for _, u := range hits {
				names = append(names, fmt.Sprintf("%s (%s)", u.Name, u.Username))
			}
			return users.User{}, userErrorf("%q matches more than one person: %s. Use a username.", who, strings.Join(names, ", "))
		}
	}
	return users.User{}, userErrorf("No one matches %q. Use a sign-in username, email address or full name.", who)
}

type PersonInfo struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
}

func personInfo(u users.User) PersonInfo {
	return PersonInfo{Name: u.Name, Username: u.Username, Email: u.Email}
}

// --- Recorded leave ---------------------------------------------------------

type RecordLeaveIn struct {
	Person string `json:"person" jsonschema:"who took the leave: the person's sign-in username, email address or exact full name"`
	Type   string `json:"type" jsonschema:"the leave type's code or name; a retired type is allowed"`
	Start  string `json:"start" jsonschema:"first day, YYYY-MM-DD; the leave must already have started"`
	End    string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD; default the same as start"`
	Note   string `json:"note" jsonschema:"why it is being recorded; required"`
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
	t, err := d.resolveTypeIn(ctx, in.Type, false)
	if err != nil {
		return RecordedOut{}, "", err
	}
	end := strings.TrimSpace(in.End)
	if end == "" {
		end = strings.TrimSpace(in.Start)
	}
	req, err := d.leave.RecordLeave(ctx, u, who.ID, leave.Input{TypeID: t.ID, Start: strings.TrimSpace(in.Start), End: end, Note: in.Note})
	if err != nil {
		return RecordedOut{}, "", err
	}
	out := RecordedOut{Request: requestInfo(req), BelowZero: req.AllowNegative}
	cycle, err := leave.ParseDate(req.CycleStart)
	if err != nil {
		return out, "", err
	}
	b, err := d.leave.BalanceIn(ctx, who.ID, t, cycle)
	if err != nil {
		return out, "", err
	}
	bi := balanceInfo(b)
	out.Balance = &bi
	out.Message = fmt.Sprintf("Recorded %d %s of %s for %s. They have been emailed and it is on the shared calendar.",
		req.Days, plural(req.Days, "day", "days"), req.TypeName, req.UserName)
	if out.BelowZero {
		out.Message += " Their balance is now below zero."
	}
	return out, out.Message, nil
}

func (d *deps) removeRecorded(ctx context.Context, u users.User, in IDIn) (RequestOut, string, error) {
	if err := requireAdmin(u); err != nil {
		return RequestOut{}, "", err
	}
	req, err := d.load(ctx, u, in.ID)
	if err != nil {
		return RequestOut{}, "", err
	}
	if req.RecordedBy == "" {
		return RequestOut{}, "", userError("Only leave an administrator recorded can be removed this way; ordinary leave is cancelled through approval.")
	}
	if _, err := d.leave.Unrecord(ctx, u, req.ID); err != nil {
		return RequestOut{}, "", err
	}
	return d.after(ctx, req.ID, "Recorded leave removed. The days are back in the balance and the person has been emailed.")
}

// --- Balances ---------------------------------------------------------------

type PersonBalancesIn struct {
	Person string `json:"person" jsonschema:"the person's sign-in username, email address or exact full name"`
}

type AdjustmentInfo struct {
	Type       string `json:"type"`
	CycleStart string `json:"cycle_start"`
	Days       int    `json:"days" jsonschema:"signed: negative takes days away"`
	Reason     string `json:"reason"`
	By         string `json:"by"`
	CreatedAt  string `json:"created_at"`
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
	b, _, err := d.myBalances(ctx, who, none{})
	if err != nil {
		return PersonBalancesOut{}, "", err
	}
	as, err := d.leave.Adjustments(ctx, who.ID)
	if err != nil {
		return PersonBalancesOut{}, "", err
	}
	out := PersonBalancesOut{Person: personInfo(who), Balances: b.Balances, Adjustments: adjustmentInfos(as)}
	return out, fmt.Sprintf("%s: %d leave types, %d %s.", who.Name, len(out.Balances), len(out.Adjustments),
		plural(len(out.Adjustments), "adjustment", "adjustments")), nil
}

type AdjustIn struct {
	Person     string `json:"person" jsonschema:"the person's sign-in username, email address or exact full name"`
	Type       string `json:"type" jsonschema:"the leave type's code or name, as listed by person_balances"`
	CycleStart string `json:"cycle_start,omitempty" jsonschema:"first day of the cycle to adjust, YYYY-MM-DD, as person_balances shows; default the current cycle"`
	Days       int    `json:"days" jsonschema:"signed whole days to add (carry-over, pro-rata) or, negative, to remove; not zero"`
	Reason     string `json:"reason" jsonschema:"why; required, kept on the person's record"`
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
	t, err := d.resolveType(ctx, in.Type)
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
	if err := d.leave.AddAdjustment(ctx, u.ID, a); err != nil {
		return AdjustOut{}, "", err
	}
	out := AdjustOut{Person: personInfo(who)}
	if c, err := leave.ParseDate(cycle); err == nil {
		if b, err := d.leave.BalanceIn(ctx, who.ID, t, c); err == nil {
			bi := balanceInfo(b)
			out.Balance = &bi
		}
	}
	as, err := d.leave.Adjustments(ctx, who.ID)
	if err != nil {
		return out, "", err
	}
	out.Adjustments = adjustmentInfos(as)
	out.Message = fmt.Sprintf("Adjusted %s's %s balance for the cycle from %s by %+d %s. Nobody is emailed.",
		who.Name, t.Name, cycle, in.Days, plural(in.Days, "day", "days"))
	return out, out.Message, nil
}

// --- Permission to go below zero -------------------------------------------

type PermissionItem struct {
	Request       RequestInfo `json:"request"`
	ShortfallDays int         `json:"shortfall_days" jsonschema:"how far below zero it would take the balance, as it stands now"`
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
	add(d, s, &mcp.Tool{Name: "person_balances", Annotations: readOnly("A person's balances (administrators)"),
		Description: "Administrators only: one person's balances for the current cycle, with the cycle start dates, and every balance adjustment made for them."}, d.personBalances)
	add(d, s, &mcp.Tool{Name: "permission_queue", Annotations: readOnly("Requests waiting for HR (administrators)"),
		Description: "Administrators only: requests waiting for HR to allow the balance to go below zero (awaiting_hr), oldest first, with how far below zero each would go."}, d.permissionQueue)

	add(d, s, &mcp.Tool{Name: "record_leave", Annotations: additive("Record leave a person took (administrators)"),
		Description: "Administrators only: write down leave a person has already started or taken, stored as approved. The person is emailed and it goes on the shared calendar. " +
			"A note saying why is required. Leave that has not started is refused; a retired type is allowed; the balance may go below zero (reported as below_zero). " +
			"You cannot record your own leave; another administrator does that."}, d.recordLeave)
	add(d, s, &mcp.Tool{Name: "remove_recorded_leave", Annotations: destructive("Remove recorded leave (administrators)"),
		Description: "Administrators only: remove leave an administrator recorded (not ordinary approved leave). The days return to the balance, the calendar event goes and the person is emailed. Cannot be undone."}, d.removeRecorded)
	add(d, s, &mcp.Tool{Name: "adjust_balance", Annotations: additive("Adjust a balance (administrators)"),
		Description: "Administrators only: add or remove days on a person's balance for one leave type and cycle (carry-over, pro-rata, correction). A reason is required. " +
			"The cycle defaults to the current one. Nobody is emailed. An adjustment cannot be edited or deleted afterwards, only offset with another."}, d.adjustBalance)
	add(d, s, &mcp.Tool{Name: "grant_negative", Annotations: additive("Allow a negative balance (administrators)"),
		Description: "Administrators only: allow a request waiting for HR to take the balance below zero. It returns to the person as a draft for them to submit, and they are emailed with your note. " +
			"You cannot grant your own request. Only when the person has told you to."}, d.grantNegative)
	add(d, s, &mcp.Tool{Name: "refuse_negative", Annotations: destructive("Refuse a negative balance (administrators)"),
		Description: "Administrators only: refuse a request waiting for HR. It ends as declined, the person is emailed your note, and it cannot be undone. A note saying why is required. " +
			"You cannot refuse your own request. Only when the person has told you to."}, d.refuseNegative)
	add(d, s, &mcp.Tool{Name: "reassign_request", Annotations: destructive("Reassign a request (administrators)"),
		Description: "Administrators only: move a pending request to another approver (or, with a blank approver, to the leave administrators). The new approver is emailed. Only for requests still pending."}, d.reassign)
}
