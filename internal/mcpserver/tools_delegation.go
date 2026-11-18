package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

// Stand-ins while an approver is away. One tool each for setting, reading and
// ending, open to everyone for their own approvals (user-confirmed); the
// optional approver parameter is for administrators acting for someone else.
// It is one tool rather than a self and an admin variant: leave.Service refuses
// a non-admin naming anyone but themselves, so the permission check stays in
// one place, and the tool only adds a clearer message first.

const delegationNote = " A stand-in covers at most 30 days (first and last day both counted), starts today or later, and an approver has one stand-in at a time. " +
	"Anyone who can sign in can be named. The stand-in can approve or decline what comes to the approver, but never the leave of whoever set the stand-in up, " +
	"and never the leave administrators' queue; the approver can still decide too, and what is undecided afterwards stays with the approver."

type DelegationInfo struct {
	ID        int64  `json:"id"`
	Approver  string `json:"approver" jsonschema:"whose approvals the stand-in covers"`
	StandIn   string `json:"stand_in"`
	Start     string `json:"start" jsonschema:"first day, YYYY-MM-DD"`
	End       string `json:"end" jsonschema:"last day, YYYY-MM-DD, inclusive"`
	Note      string `json:"note,omitempty"`
	SetUpBy   string `json:"set_up_by,omitempty" jsonschema:"set when someone other than the approver arranged it"`
	Active    bool   `json:"active" jsonschema:"true when it covers today"`
	RevokedBy string `json:"revoked_by,omitempty" jsonschema:"set when it was ended early: who ended it"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

func (d *deps) delegationInfo(x leave.Delegation) DelegationInfo {
	info := DelegationInfo{ID: x.ID, Approver: x.DelegatorName, StandIn: x.DelegateName, Start: x.Start, End: x.End,
		Note: x.Note, Active: x.Active(d.leave.Today().Format(time.DateOnly)), RevokedBy: x.RevokedBy, RevokedAt: x.RevokedAt}
	if x.CreatedByID != x.DelegatorID {
		info.SetUpBy = x.CreatedBy
	}
	return info
}

// approverFor is whose approvals a tool acts on: the caller's own, or, for an
// administrator naming someone, theirs.
func (d *deps) approverFor(ctx context.Context, u users.User, who string) (users.User, error) {
	if who == "" {
		return u, nil
	}
	if !u.Admin {
		// Naming yourself is the same as leaving it out.
		if strings.EqualFold(who, u.Username) || strings.EqualFold(who, u.Email) || strings.EqualFold(who, u.Name) {
			return u, nil
		}
		return users.User{}, userError("Only administrators can act for someone else. Leave approver out to use your own approvals.")
	}
	return d.resolvePerson(ctx, who)
}

type DelegateIn struct {
	StandIn  string `json:"delegate" jsonschema:"the stand-in: their sign-in username, email address or exact full name"`
	Start    string `json:"start" jsonschema:"first day, YYYY-MM-DD; today or later"`
	End      string `json:"end" jsonschema:"last day, YYYY-MM-DD; at most 30 days from the first, both counted"`
	Note     string `json:"note,omitempty"`
	Approver string `json:"approver,omitempty" jsonschema:"administrators only: set the stand-in up for this person (username, email or full name) instead of yourself"`
}

type DelegationOut struct {
	Delegation DelegationInfo `json:"delegation"`
	Message    string         `json:"message"`
}

func (d *deps) delegateApprovals(ctx context.Context, u users.User, in DelegateIn) (DelegationOut, string, error) {
	approver, err := d.approverFor(ctx, u, strings.TrimSpace(in.Approver))
	if err != nil {
		return DelegationOut{}, "", err
	}
	standIn, err := d.resolvePerson(ctx, in.StandIn)
	if err != nil {
		return DelegationOut{}, "", err
	}
	x, err := d.leave.Delegate(ctx, u, approver.ID, standIn.ID, in.Start, in.End, in.Note)
	if errors.Is(err, leave.ErrNotAllowed) {
		return DelegationOut{}, "", userError("That is not possible: only the approver or an administrator can set up a stand-in.")
	}
	if err != nil {
		return DelegationOut{}, "", err
	}
	who := "you"
	if approver.ID != u.ID {
		who = approver.Name
	}
	msg := fmt.Sprintf("%s will approve for %s from %s to %s. They have been emailed.", x.DelegateName, who, x.Start, x.End)
	return DelegationOut{Delegation: d.delegationInfo(x), Message: msg}, msg, nil
}

type MyDelegationsIn struct {
	Approver string `json:"approver,omitempty" jsonschema:"administrators only: look at this person's stand-ins instead of your own"`
}

type MyDelegationsOut struct {
	ForThem []DelegationInfo `json:"stand_ins_for_them" jsonschema:"current and upcoming stand-ins the person has named for their own approvals"`
	ByThem  []DelegationInfo `json:"standing_in" jsonschema:"current and upcoming delegations that name the person as the stand-in"`
	Past    []DelegationInfo `json:"past" jsonschema:"their last 20 stand-ins that are over or were ended early"`
}

func (d *deps) myDelegations(ctx context.Context, u users.User, in MyDelegationsIn) (MyDelegationsOut, string, error) {
	p, err := d.approverFor(ctx, u, strings.TrimSpace(in.Approver))
	if err != nil {
		return MyDelegationsOut{}, "", err
	}
	infos := func(f leave.DelegationFilter) ([]DelegationInfo, error) {
		xs, err := d.leave.Delegations(ctx, f)
		out := make([]DelegationInfo, 0, len(xs))
		for _, x := range xs {
			out = append(out, d.delegationInfo(x))
		}
		return out, err
	}
	var out MyDelegationsOut
	if out.ForThem, err = infos(leave.DelegationFilter{DelegatorID: p.ID}); err != nil {
		return out, "", err
	}
	if out.ByThem, err = infos(leave.DelegationFilter{DelegateID: p.ID}); err != nil {
		return out, "", err
	}
	if out.Past, err = infos(leave.DelegationFilter{DelegatorID: p.ID, Past: true}); err != nil {
		return out, "", err
	}
	return out, fmt.Sprintf("%d current or upcoming stand-in(s) for %s; standing in for %d.", len(out.ForThem), p.Name, len(out.ByThem)), nil
}

type RevokeDelegationIn struct {
	ID int64 `json:"id" jsonschema:"the delegation's id, from my_delegations"`
}

func (d *deps) revokeDelegation(ctx context.Context, u users.User, in RevokeDelegationIn) (DelegationOut, string, error) {
	x, err := d.leave.RevokeDelegation(ctx, u, in.ID)
	if errors.Is(err, leave.ErrNotAllowed) {
		return DelegationOut{}, "", userError("That stand-in does not exist, has already ended, or is not yours to end.")
	}
	if err != nil {
		return DelegationOut{}, "", err
	}
	msg := fmt.Sprintf("%s no longer approves for %s. They have been emailed.", x.DelegateName, x.DelegatorName)
	return DelegationOut{Delegation: d.delegationInfo(x), Message: msg}, msg, nil
}

func (d *deps) registerDelegation(s *mcp.Server) {
	add(d, s, &mcp.Tool{Name: "my_delegations", Annotations: readOnly("My stand-ins"),
		Description: "Your stand-ins for approvals (the people who approve for you while you are away), who you are standing in for, and your recent past ones, with the ids revoke_delegation takes. Administrators can name another person with approver."}, d.myDelegations)
	add(d, s, &mcp.Tool{Name: "delegate_approvals", Annotations: additive("Name a stand-in for approvals"),
		Description: "Name someone to approve or decline, in your place, the leave that comes to you while you are away, for a date range. The stand-in is emailed (and the approver too when an administrator sets it up). " +
			"It is available to everyone for their own approvals. Administrators can set one up for another person with approver." + delegationNote +
			" Only when the person has asked for it."}, d.delegateApprovals)
	add(d, s, &mcp.Tool{Name: "revoke_delegation", Annotations: destructive("End a stand-in"),
		Description: "End a stand-in now, so they can no longer approve for the approver. Only the approver or an administrator can. The stand-in is emailed. Cannot be undone: a new stand-in has to be named again. Only when the person has asked for it."}, d.revokeDelegation)
}
