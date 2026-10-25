// Package mcpserver exposes the leave app to AI assistants over MCP.
//
// Every tool runs as the person the bearer token belongs to and goes through
// the same leave.Service methods, users.Store lookups and permission helpers
// (leave.CanView, leave.CanDecide, leave.CanWithdraw, users.User.Admin) as the
// pages do. There is no permission logic in this package: a tool that would do
// something the person cannot do on the website gets the same refusal.
//
// A refusal the person should hear about (a FieldErrors, ErrNotAllowed, a
// request that is not theirs to see) comes back as a tool result with
// IsError set and the human message, not as a protocol error, so the assistant
// can relay it. Anything unexpected is logged here and reported as a generic
// failure: internals do not belong in a chat.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

type Config struct {
	Leave  *leave.Service
	Users  *users.Store
	Outbox *outbox.Store
	// Verifier checks bearer tokens; see oauth.Server.Verify.
	Verifier auth.TokenVerifier
	// ResourceMetadataURL goes in the WWW-Authenticate header of a 401, which is
	// how a client that has no token finds out where to sign in.
	ResourceMetadataURL string
	// Scopes every token must carry.
	Scopes []string
}

const instructions = `This is the staff leave system. Everything you do here is done as the signed-in person, with exactly their permissions.
Use my_balances to see leave types and what is left, and preview_request before apply_for_leave to see the day count and any rule that would stop the request.
Never apply for, withdraw, approve or decline leave, and never record or remove recorded leave, adjust a balance, grant or refuse a negative balance, or reassign a request, unless the person has clearly asked you to. Dates are YYYY-MM-DD.`

// New returns the MCP endpoint: Streamable HTTP, behind bearer-token auth.
//
// Stateless, because every call carries its own token and nothing here needs
// to call the client back; there is no session to hold, expire or hijack.
// JSON responses rather than an event stream, so a reverse proxy has nothing
// to buffer or time out.
//
// The SDK's DNS-rebinding guard is left on (see the README): it refuses a
// request that arrives on a loopback address with a non-loopback Host.
func New(cfg Config) http.Handler {
	s := mcp.NewServer(&mcp.Implementation{Name: "leave", Title: "Leave", Version: "1.0.0"},
		&mcp.ServerOptions{Instructions: instructions})
	d := &deps{leave: cfg.Leave, users: cfg.Users, outbox: cfg.Outbox}
	d.register(s)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return auth.RequireBearerToken(cfg.Verifier, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: cfg.ResourceMetadataURL, Scopes: cfg.Scopes})(h)
}

type deps struct {
	leave  *leave.Service
	users  *users.Store
	outbox *outbox.Store
}

// userError is a message meant for the person, whole.
type userError string

func (e userError) Error() string { return string(e) }

func userErrorf(format string, a ...any) error { return userError(fmt.Sprintf(format, a...)) }

// humanError turns what the service returned into what the assistant is told.
func humanError(tool string, err error) error {
	var fe leave.FieldErrors
	var ue userError
	switch {
	case errors.As(err, &ue):
		return ue
	case errors.As(err, &fe):
		keys := sortedKeys(fe)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fieldLabel(k)+fe[k])
		}
		return userError(strings.Join(parts, " "))
	case errors.Is(err, leave.ErrChanged):
		return userError("This request was changed after you read it. Read it again (get_request) and check before deciding.")
	case errors.Is(err, leave.ErrNotAllowed):
		return userError("That is not possible. The request may already have been decided or withdrawn, " +
			"or it is not yours to do this with.")
	case errors.Is(err, leave.ErrNotFound):
		return errNotFound
	}
	slog.Error("mcp tool failed", "tool", tool, "err", err)
	return userError("Something went wrong on the server. Please try again, and tell the office if it keeps happening.")
}

// fieldLabel prefixes a message with its field where the message alone would
// not say what it is about.
func fieldLabel(field string) string {
	switch field {
	case "note":
		return "Note: "
	case "type":
		return "Leave type: "
	case "start":
		return "Start date: "
	case "end":
		return "End date: "
	case "person":
		return "Person: "
	case "approver":
		return "Approver: "
	case "cycle":
		return "Cycle: "
	case "days":
		return "Days: "
	case "reason":
		return "Reason: "
	}
	return ""
}

// errNotFound is what someone else's request looks like, the same as a missing
// one: the pages answer 404 for both, and so does this.
var errNotFound = userError("No such request.")

// currentUser loads the person the token belongs to. The token verifier has
// already refused a disabled user; this reads the same row the pages would.
func (d *deps) currentUser(ctx context.Context, req *mcp.CallToolRequest) (users.User, error) {
	var ti *auth.TokenInfo
	if x := req.GetExtra(); x != nil {
		ti = x.TokenInfo
	}
	if ti == nil {
		ti = auth.TokenInfoFromContext(ctx)
	}
	if ti == nil {
		return users.User{}, userError("Not signed in.")
	}
	id, err := strconv.ParseInt(ti.UserID, 10, 64)
	if err != nil {
		return users.User{}, userError("Not signed in.")
	}
	u, err := d.users.ByID(ctx, id)
	if err != nil || u.Disabled {
		return users.User{}, userError("This account can no longer use the leave system.")
	}
	return u, nil
}

// add registers a tool whose handler runs as the token's user and returns its
// output, a one-line summary, and an error. The result's text is the summary
// followed by the output as JSON: clients differ in whether they show the
// structured content to the model, and the text is what all of them show.
func add[In, Out any](d *deps, s *mcp.Server, t *mcp.Tool,
	f func(ctx context.Context, u users.User, in In) (Out, string, error)) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		u, err := d.currentUser(ctx, req)
		if err != nil {
			return nil, zero, err
		}
		out, summary, err := f(ctx, u, in)
		if err != nil {
			return nil, zero, humanError(t.Name, err)
		}
		j, err := json.Marshal(out)
		if err != nil {
			return nil, zero, humanError(t.Name, err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: summary + "\n\n" + string(j)}}}, out, nil
	})
}

func requireAdmin(u users.User) error {
	if !u.Admin {
		return userError("This needs a leave administrator, and you are not one.")
	}
	return nil
}

// load fetches a request the person may see. One they may not is reported as
// missing, as the pages do.
func (d *deps) load(ctx context.Context, u users.User, id int64) (leave.Request, error) {
	if id <= 0 {
		return leave.Request{}, errNotFound
	}
	r, err := d.leave.Get(ctx, id)
	if errors.Is(err, leave.ErrNotFound) || (err == nil && !leave.CanView(u, r)) {
		return leave.Request{}, errNotFound
	}
	return r, err
}

func ptr[T any](v T) *T { return &v }

// Annotations. Clients use them to decide when to ask the person first, so
// they are set on every tool rather than left to the spec's defaults (which
// treat an unmarked tool as destructive).
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: ptr(false)}
}

func additive(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
}

func destructive(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
}

func (d *deps) register(s *mcp.Server) {
	// Read
	add(d, s, &mcp.Tool{Name: "my_balances", Annotations: readOnly("My leave balances"),
		Description: "Your leave balances for the current cycle, one row per leave type, with the type's code and unit."}, d.myBalances)
	add(d, s, &mcp.Tool{Name: "my_requests", Annotations: readOnly("My leave requests"),
		Description: "Your own leave requests and cancellations, newest first. Optionally only one status."}, d.myRequests)
	add(d, s, &mcp.Tool{Name: "get_request", Annotations: readOnly("Look at a request"),
		Description: "One request in full: its decision trail, any department or team coverage warnings, and which actions are open to you. " +
			"Works for your own requests, ones routed to you, ones you decided, and (for administrators) any."}, d.getRequest)
	add(d, s, &mcp.Tool{Name: "team_leave", Annotations: readOnly("Team leave"),
		Description: "Current and upcoming approved or pending leave of the people who report to you."}, d.teamLeave)
	add(d, s, &mcp.Tool{Name: "public_holidays", Annotations: readOnly("Public holidays"),
		Description: "Public holidays for a year (default: this year). Leave does not cost these days."}, d.publicHolidays)
	add(d, s, &mcp.Tool{Name: "preview_request", Annotations: readOnly("Preview a leave request"),
		Description: "Check a leave request without making it: the number of days it costs, your balance afterwards, and every rule that would stop it " +
			"(dates, restricted day, overlap, department and team coverage, balance). Also says whether you may ask for special permission or HR's " +
			"permission to go below zero. Call this before apply_for_leave."}, d.previewRequest)

	// Act
	add(d, s, &mcp.Tool{Name: "apply_for_leave", Annotations: additive("Apply for leave"),
		Description: "Submit a leave request for approval, exactly as the Apply form does; the approver is emailed. " +
			"Only call this when the person has asked for it. Set override only after preview_request says special permission is available and the " +
			"person wants it (the request then goes up every approval level); set ask_hr only when the balance is the only problem and the person wants HR's permission to go below zero."}, d.apply)
	add(d, s, &mcp.Tool{Name: "edit_request", Annotations: additive("Edit a pending request"),
		Description: "Change the type, dates or note of one of your own pending requests that nobody has started to decide, instead of withdrawing it and applying again. " +
			"Give the id and the full new type, start, end and note; the same checks as apply_for_leave apply, and its approver is emailed. " +
			"Only call this when the person has asked for it. override asks for special permission as in apply_for_leave. There is no ask_hr here: to go below zero, withdraw and apply again with ask_hr."}, d.edit)
	add(d, s, &mcp.Tool{Name: "withdraw_request", Annotations: destructive("Withdraw a request"),
		Description: "Withdraw one of your own requests that has not been decided yet (pending, waiting for HR, or a draft). Cannot be undone."}, d.withdraw)
	add(d, s, &mcp.Tool{Name: "submit_draft", Annotations: additive("Submit a draft request"),
		Description: "Submit one of your draft requests: one made for you for a company shutdown, or one HR allowed to go below zero. " +
			"A blank note keeps the draft's own. override asks for special permission if the draft is over a department limit."}, d.submitDraft)
	add(d, s, &mcp.Tool{Name: "request_cancellation", Annotations: additive("Ask to cancel approved leave"),
		Description: "Ask for approved leave that has not started to be cancelled. It goes through approval; the leave stays booked until approved."}, d.requestCancellation)

	// Approve
	add(d, s, &mcp.Tool{Name: "approval_queue", Annotations: readOnly("Approval queue"),
		Description: "Requests waiting for your decision, oldest first, each with any department or team coverage warnings, who it goes to next if you approve, the requester's balance afterwards and its trail so far."}, d.approvalQueue)
	add(d, s, &mcp.Tool{Name: "decide_request", Annotations: destructive("Approve or decline a request"),
		Description: "Approve or decline a request waiting for your decision. A decline needs a note saying why. " +
			"Only call this when the person has told you the decision; never decide on your own judgement. " +
			"Pass the request's fingerprint (from get_request or approval_queue) as expected, so that a request its owner has edited since you read it is not decided unseen."}, d.decide)

	// Admin, read-only
	add(d, s, &mcp.Tool{Name: "all_requests", Annotations: readOnly("All requests (administrators)"),
		Description: "Administrators only: every leave request, newest first, filtered by status, dates (requests overlapping the range) and person."}, d.allRequests)
	add(d, s, &mcp.Tool{Name: "who_is_away", Annotations: readOnly("Who is away (administrators)"),
		Description: "Administrators only: who has approved or pending leave in a date range, grouped by department."}, d.whoIsAway)
	add(d, s, &mcp.Tool{Name: "outbox_problems", Annotations: readOnly("Failing emails and calendar jobs (administrators)"),
		Description: "Administrators only: notification emails and calendar jobs that are failing or have given up."}, d.outboxProblems)

	d.registerAdminActions(s)
}
