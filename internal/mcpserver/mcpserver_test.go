package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

type fixture struct {
	t   *testing.T
	srv *httptest.Server
	svc *leave.Service
	us  *users.Store
	db  *sql.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	dir, err := directory.LoadFake("../../testdata/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := dir.AllUsers(ctx)
	if _, _, err := us.Sync(ctx, all); err != nil {
		t.Fatal(err)
	}
	if ts, err := dir.Teams(ctx); err != nil {
		t.Fatal(err)
	} else if _, _, err := us.SyncTeams(ctx, "fake", ts); err != nil {
		t.Fatal(err)
	}
	pastor, _ := us.ByUsername(ctx, "pastor")
	hr, _ := us.ByUsername(ctx, "hr")
	st, _ := svc.Settings(ctx)
	st.FinalApproverID = pastor.ID
	if err := svc.SaveSettings(ctx, hr, st); err != nil {
		t.Fatal(err)
	}

	// The token is the username: this file tests the tools, and the real
	// token path is tested by the end-to-end test in package main.
	verify := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		u, err := us.ByUsername(ctx, token)
		if err != nil || u.Disabled {
			return nil, fmt.Errorf("%w: no such token", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{UserID: fmt.Sprint(u.ID), Scopes: []string{"leave"}, Expiration: time.Now().Add(time.Hour)}, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", New(Config{Leave: svc, Users: us, Outbox: outbox.NewStore(d), Verifier: verify,
		ResourceMetadataURL: "http://leave.test/.well-known/oauth-protected-resource", Scopes: []string{"leave"}}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{t: t, srv: srv, svc: svc, us: us, db: d}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// as connects an MCP client as the named person.
func (f *fixture) as(username string) *mcp.ClientSession {
	f.t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{username}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		f.t.Fatalf("connect as %s: %v", username, err)
	}
	f.t.Cleanup(func() { cs.Close() })
	return cs
}

type result struct {
	IsError bool
	Text    string
	Data    map[string]any
}

func (f *fixture) call(cs *mcp.ClientSession, tool string, args map[string]any) result {
	f.t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		f.t.Fatalf("%s: protocol error: %v", tool, err)
	}
	r := result{IsError: res.IsError}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			r.Text += tc.Text
		}
	}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &r.Data)
	}
	return r
}

func (f *fixture) ok(cs *mcp.ClientSession, tool string, args map[string]any) result {
	f.t.Helper()
	r := f.call(cs, tool, args)
	if r.IsError {
		f.t.Fatalf("%s failed: %s", tool, r.Text)
	}
	return r
}

func (f *fixture) refused(cs *mcp.ClientSession, tool string, args map[string]any, contains string) {
	f.t.Helper()
	r := f.call(cs, tool, args)
	if !r.IsError || !strings.Contains(r.Text, contains) {
		f.t.Fatalf("%s: isError=%v text=%q, want a tool error containing %q", tool, r.IsError, r.Text, contains)
	}
	if r.Data != nil {
		f.t.Fatalf("%s: a refusal carried structured data: %v", tool, r.Data)
	}
}

func get(m map[string]any, path ...string) any {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[p]
	}
	return v
}

func id(r result) int64 { return int64(get(r.Data, "request", "id").(float64)) }

func TestToolAnnotations(t *testing.T) {
	f := newFixture(t)
	tools, err := f.as("staff1").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{"my_balances": true, "my_requests": true, "get_request": true, "team_leave": true,
		"public_holidays": true, "preview_request": true, "approval_queue": true, "all_requests": true,
		"who_is_away": true, "outbox_problems": true}
	for _, n := range []string{"person_balances", "permission_queue"} {
		readOnly[n] = true
	}
	// grant_negative commits the person to a decision that cannot be recalled
	// (the request is a draft they may submit), so clients are told to ask first.
	destructiveTools := map[string]bool{"decide_request": true, "withdraw_request": true,
		"remove_recorded_leave": true, "refuse_negative": true, "reassign_request": true, "grant_negative": true}
	changing := map[string]bool{"apply_for_leave": true, "submit_draft": true, "request_cancellation": true,
		"record_leave": true, "adjust_balance": true}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if tool.OutputSchema == nil {
			t.Errorf("%s has no output schema", tool.Name)
		}
		switch {
		case readOnly[tool.Name]:
			if !a.ReadOnlyHint {
				t.Errorf("%s should be read-only", tool.Name)
			}
		case destructiveTools[tool.Name]:
			if a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
				t.Errorf("%s should be marked destructive: %+v", tool.Name, a)
			}
		case changing[tool.Name]:
			if a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
				t.Errorf("%s should change state without being destructive: %+v", tool.Name, a)
			}
		default:
			t.Errorf("unexpected tool %s", tool.Name)
		}
	}
	for _, m := range []map[string]bool{readOnly, destructiveTools, changing} {
		for name := range m {
			if !seen[name] {
				t.Errorf("tool %s is not registered", name)
			}
		}
	}
}

func TestAnonymousCallIsPointedAtSignIn(t *testing.T) {
	f := newFixture(t)
	for _, token := range []string{"", "nobody"} {
		req, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("WWW-Authenticate"); res.StatusCode != 401 || !strings.Contains(got, "resource_metadata=") {
			t.Errorf("token %q: %d %q", token, res.StatusCode, got)
		}
	}
}

func TestApplyWithdrawAndStructuredResults(t *testing.T) {
	f := newFixture(t)
	staff := f.as("staff1")

	bal := f.ok(staff, "my_balances", nil)
	var annual map[string]any
	for _, b := range bal.Data["balances"].([]any) {
		if b.(map[string]any)["code"] == "annual" {
			annual = b.(map[string]any)
		}
	}
	if annual == nil || annual["available"].(float64) != 15 || annual["unit"] != "workdays" {
		t.Fatalf("balances = %v", bal.Data)
	}
	if !strings.Contains(bal.Text, "leave types") || !strings.Contains(bal.Text, `"balances"`) {
		t.Errorf("text result lacks the summary or the data: %q", bal.Text)
	}

	args := map[string]any{"type": "Annual leave", "start": "2026-10-12", "end": "2026-10-14", "note": "rest"}
	p := f.ok(staff, "preview_request", args)
	if p.Data["ok"] != true || p.Data["days"].(float64) != 3 || p.Data["available_after"].(float64) != 12 ||
		p.Data["approver"] != "Olivia Operations" || len(p.Data["problems"].([]any)) != 0 {
		t.Fatalf("preview = %v", p.Data)
	}
	// Previewing made nothing.
	if rs := f.ok(staff, "my_requests", nil); len(rs.Data["requests"].([]any)) != 0 {
		t.Fatalf("preview created a request: %v", rs.Data)
	}

	args["type"] = "annual" // the code works as well as the name
	r := f.ok(staff, "apply_for_leave", args)
	if get(r.Data, "request", "status") != "pending" || get(r.Data, "request", "approver") != "Olivia Operations" ||
		get(r.Data, "request", "days").(float64) != 3 || !strings.Contains(r.Text, "Olivia Operations") {
		t.Fatalf("apply = %v / %s", r.Data, r.Text)
	}
	reqID := id(r)

	// The same rule the page enforces: the dates are taken now.
	f.refused(staff, "apply_for_leave", args, "already have leave booked")

	d := f.ok(staff, "get_request", map[string]any{"id": reqID})
	if d.Data["can_withdraw"] != true || d.Data["can_decide"] != false || d.Data["can_submit"] != false {
		t.Fatalf("get_request = %v", d.Data)
	}
	if mine := f.ok(staff, "my_requests", map[string]any{"status": "pending"}); len(mine.Data["requests"].([]any)) != 1 {
		t.Fatalf("my_requests = %v", mine.Data)
	}
	f.refused(staff, "my_requests", map[string]any{"status": "bogus"}, "Unknown status")

	w := f.ok(staff, "withdraw_request", map[string]any{"id": reqID})
	if get(w.Data, "request", "status") != "withdrawn" {
		t.Fatalf("withdraw = %v", w.Data)
	}
	// ErrNotAllowed comes back as a message for the person, not a protocol error.
	f.refused(staff, "withdraw_request", map[string]any{"id": reqID}, "That is not possible")
}

func TestDomainRefusalsAreToolErrors(t *testing.T) {
	f := newFixture(t)
	staff := f.as("staff1")
	f.refused(staff, "apply_for_leave", map[string]any{"type": "nonsense", "start": "2026-10-12"}, "annual (Annual leave)")
	f.refused(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "not a date"}, "Start date: Enter the first day")
	f.refused(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-06-01"}, "can start at most 92 days ago")
	f.refused(staff, "apply_for_leave", map[string]any{"type": "family", "start": "2026-10-13"}, "Note: Please add a short note")
	// A missing required argument is the SDK's own validation, also a tool error.
	f.refused(staff, "apply_for_leave", map[string]any{"start": "2026-10-13"}, "validating")
	f.refused(staff, "get_request", map[string]any{"id": 99999}, "No such request")
}

func TestPreviewOffersWhatTheButtonsOffer(t *testing.T) {
	f := newFixture(t)
	staff := f.as("staff1")
	// About three months of working days against fifteen.
	args := map[string]any{"type": "annual", "start": "2026-11-02", "end": "2027-01-29", "note": "long trip"}
	p := f.ok(staff, "preview_request", args)
	if p.Data["ok"] != false || p.Data["can_ask_hr"] != true || p.Data["can_ask_special_permission"] != false ||
		p.Data["shortfall_days"].(float64) <= 0 || len(p.Data["problems"].([]any)) != 1 {
		t.Fatalf("preview = %v", p.Data)
	}
	// Without ask_hr it is refused; with it, the request waits for HR.
	f.refused(staff, "apply_for_leave", args, "you have 15 left")
	args["ask_hr"] = true
	r := f.ok(staff, "apply_for_leave", args)
	if get(r.Data, "request", "status") != "awaiting_hr" || !strings.Contains(r.Text, "HR") {
		t.Fatalf("ask_hr = %v", r.Data)
	}

	// HR allows it (through the service, as the admin page does); the draft is the person's to submit.
	ctx := context.Background()
	hr, _ := f.us.ByUsername(ctx, "hr")
	if _, err := f.svc.GrantNegative(ctx, hr, id(r), "ok"); err != nil {
		t.Fatal(err)
	}
	d := f.ok(staff, "get_request", map[string]any{"id": id(r)})
	if get(d.Data, "request", "status") != "draft" || d.Data["can_submit"] != true || d.Data["can_withdraw"] != true {
		t.Fatalf("draft = %v", d.Data)
	}
	s := f.ok(staff, "submit_draft", map[string]any{"id": id(r)})
	if get(s.Data, "request", "status") != "pending" {
		t.Fatalf("submit_draft = %v", s.Data)
	}
	f.refused(staff, "submit_draft", map[string]any{"id": id(r)}, "That is not possible")
}

func TestApprovalsGoThroughTheServiceRules(t *testing.T) {
	f := newFixture(t)
	staff, head, pastor := f.as("staff1"), f.as("headops"), f.as("pastor")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14"})
	reqID := id(r)

	// The approver sees it in their queue; nobody else does.
	q := f.ok(head, "approval_queue", nil)
	items := q.Data["queue"].([]any)
	if len(items) != 1 || get(items[0].(map[string]any), "request", "id").(float64) != float64(reqID) {
		t.Fatalf("queue = %v", q.Data)
	}
	if q := f.ok(pastor, "approval_queue", nil); len(q.Data["queue"].([]any)) != 0 {
		t.Errorf("pastor's queue = %v", q.Data)
	}
	if tl := f.ok(head, "team_leave", nil); len(tl.Data["requests"].([]any)) != 1 {
		t.Errorf("team_leave = %v", tl.Data)
	}

	// Not the approver, and not the requester's own to approve.
	// Someone it was not routed to cannot even see it, as on the page.
	f.refused(pastor, "decide_request", map[string]any{"id": reqID, "decision": "approve"}, "No such request")
	f.refused(staff, "decide_request", map[string]any{"id": reqID, "decision": "approve"}, "That is not possible")
	f.refused(pastor, "get_request", map[string]any{"id": reqID}, "No such request")
	// An explicit word, never a default; a decline says why.
	f.refused(head, "decide_request", map[string]any{"id": reqID, "decision": "yes"}, `exactly "approve" or "decline"`)
	f.refused(head, "decide_request", map[string]any{"id": reqID, "decision": ""}, "decision")
	f.refused(head, "decide_request", map[string]any{"id": reqID, "decision": "decline"}, "Say why")
	if got := f.ok(staff, "get_request", map[string]any{"id": reqID}); get(got.Data, "request", "status") != "pending" {
		t.Fatalf("refusals changed the request: %v", got.Data)
	}

	a := f.ok(head, "decide_request", map[string]any{"id": reqID, "decision": "approve", "note": "enjoy"})
	if get(a.Data, "request", "status") != "approved" || a.Text == "" {
		t.Fatalf("approve = %v", a.Data)
	}
	f.refused(head, "decide_request", map[string]any{"id": reqID, "decision": "approve"}, "That is not possible")

	// Cancelling approved leave goes back through approval.
	c := f.ok(staff, "request_cancellation", map[string]any{"id": reqID, "note": "plans changed"})
	if get(c.Data, "request", "kind") != "cancellation" || get(c.Data, "request", "status") != "pending" ||
		get(c.Data, "request", "cancels_request_id").(float64) != float64(reqID) {
		t.Fatalf("cancellation = %v", c.Data)
	}
	orig := f.ok(staff, "get_request", map[string]any{"id": reqID})
	if orig.Data["pending_cancellation_id"].(float64) != float64(id(c)) || orig.Data["can_request_cancellation"] != false {
		t.Errorf("original = %v", orig.Data)
	}
	cd := f.ok(head, "get_request", map[string]any{"id": id(c)})
	if get(cd.Data, "cancels", "id").(float64) != float64(reqID) || cd.Data["can_decide"] != true {
		t.Errorf("cancellation detail = %v", cd.Data)
	}
	f.ok(head, "decide_request", map[string]any{"id": id(c), "decision": "decline", "note": "no"})
}

// A staff token calling an admin tool is refused, and an administrator is not.
func TestAdminToolsRefuseStaff(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14"})

	for tool, args := range map[string]map[string]any{
		"all_requests":    nil,
		"who_is_away":     {"from": "2026-10-12"},
		"outbox_problems": nil,
	} {
		f.refused(staff, tool, args, "needs a leave administrator")
		f.refused(f.as("headops"), tool, args, "needs a leave administrator")
		f.ok(hr, tool, args)
	}

	all := f.ok(hr, "all_requests", map[string]any{"status": "pending", "username": "staff1", "from": "2026-10-01", "to": "2026-10-31"})
	if rs := all.Data["requests"].([]any); len(rs) != 1 || get(rs[0].(map[string]any), "id").(float64) != float64(id(r)) {
		t.Fatalf("all_requests = %v", all.Data)
	}
	f.refused(hr, "all_requests", map[string]any{"status": "bogus"}, "Unknown status")
	f.refused(hr, "all_requests", map[string]any{"from": "yesterday"}, "from must be a date")
	f.refused(hr, "all_requests", map[string]any{"username": "ghost"}, "No one signs in as")

	away := f.ok(hr, "who_is_away", map[string]any{"from": "2026-10-13", "to": "2026-10-13", "department": "operations"})
	deps := away.Data["departments"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["department"] != "Operations" {
		t.Fatalf("who_is_away = %v", away.Data)
	}
	if away := f.ok(hr, "who_is_away", map[string]any{"from": "2026-10-13", "department": "Kids"}); len(away.Data["departments"].([]any)) != 0 {
		t.Errorf("Kids = %v", away.Data)
	}
	f.refused(hr, "who_is_away", map[string]any{"from": "2026-10-13", "to": "2026-10-01"}, "not be before")

	// A requester sees their own request but an administrator sees anyone's,
	// the same CanView rule as the request page.
	f.ok(hr, "get_request", map[string]any{"id": id(r)})
	f.refused(f.as("staff2"), "get_request", map[string]any{"id": id(r)}, "No such request")
}

func TestPublicHolidays(t *testing.T) {
	f := newFixture(t)
	staff := f.as("staff1")
	h := f.ok(staff, "public_holidays", nil)
	if h.Data["year"].(float64) != 2026 || !strings.Contains(h.Text, "Day of Reconciliation") {
		t.Fatalf("holidays = %v", h.Data)
	}
	f.ok(staff, "public_holidays", map[string]any{"year": 2027})
	f.refused(staff, "public_holidays", map[string]any{"year": 1999}, "Ask for a year between")
}

// The token verifier refuses a disabled user, and the tools check again: a
// token that outlives the account (a verifier bug) still gets nothing.
func TestDisabledUserGetsNothing(t *testing.T) {
	f := newFixture(t)
	staff := f.as("staff1")
	f.ok(staff, "my_balances", nil)
	if _, _, err := f.us.Sync(context.Background(), []directory.User{{ID: "x", DN: "CN=x", Username: "x", Name: "x"}}); err != nil {
		t.Fatal(err)
	}
	if u2, _ := f.us.ByUsername(context.Background(), "staff1"); !u2.Disabled {
		t.Fatal("sync did not disable staff1")
	}
	// The fake verifier already refuses; the connection itself is refused.
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	_, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: f.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{"staff1"}}, DisableStandaloneSSE: true}, nil)
	if err == nil {
		t.Fatal("a disabled user connected")
	}
	// The old session's next call is refused too.
	if _, err := staff.CallTool(context.Background(), &mcp.CallToolParams{Name: "my_balances"}); err == nil {
		t.Error("a disabled user's open session still works")
	}
}

// A team limit shows in the preview and the request detail as its own
// warning, next to any department one.
func TestTeamCoverageWarningsAreAList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	teams, _ := f.svc.Teams(ctx, false)
	for _, tm := range teams {
		if tm.Name == "Band" {
			if err := f.svc.SetTeamRule(ctx, tm.ID, 34); err != nil {
				t.Fatal(err)
			}
		}
	}
	args := map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14", "note": "n"}
	f.ok(f.as("headops"), "apply_for_leave", args)

	staff := f.as("staff1")
	p := f.ok(staff, "preview_request", args)
	warns, _ := p.Data["coverage_warnings"].([]any)
	if len(warns) != 1 || !strings.Contains(warns[0].(string), "the Band team (2 of 3)") || p.Data["can_ask_special_permission"] != true {
		t.Fatalf("preview = %v", p.Data)
	}
	args["override"] = true
	r := f.ok(staff, "apply_for_leave", args)
	d := f.ok(f.as("headops"), "get_request", map[string]any{"id": id(r)})
	if w, _ := d.Data["coverage_warnings"].([]any); len(w) != 1 {
		t.Fatalf("get_request = %v", d.Data)
	}
	q := f.ok(f.as("headops"), "approval_queue", nil)
	item := q.Data["queue"].([]any)[0].(map[string]any)
	if w, _ := item["coverage_warnings"].([]any); len(w) != 1 {
		t.Fatalf("queue = %v", q.Data)
	}
}

// makeAdmin makes a second administrator: the service forbids self-actions,
// so most admin tools need two to be exercised on a real target.
func (f *fixture) makeAdmin(username string) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE users SET is_admin = 1 WHERE username = ?`, username); err != nil {
		f.t.Fatal(err)
	}
}

func TestAdminActionToolsRefuseStaff(t *testing.T) {
	f := newFixture(t)
	staff, hr := f.as("staff1"), f.as("hr")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12"})
	calls := map[string]map[string]any{
		"record_leave":          {"person": "staff2", "type": "annual", "start": "2026-10-01", "note": "x"},
		"remove_recorded_leave": {"id": id(r)},
		"person_balances":       {"person": "staff2"},
		"adjust_balance":        {"person": "staff2", "type": "annual", "days": 1, "reason": "x"},
		"permission_queue":      nil,
		"grant_negative":        {"id": id(r)},
		"refuse_negative":       {"id": id(r), "note": "x"},
		"reassign_request":      {"id": id(r), "approver": "headkids"},
	}
	for tool, args := range calls {
		f.refused(staff, tool, args, "needs a leave administrator")
		f.refused(f.as("headops"), tool, args, "needs a leave administrator")
	}
	// Nothing happened to the request.
	if d := f.ok(hr, "get_request", map[string]any{"id": id(r)}); get(d.Data, "request", "approver") != "Olivia Operations" {
		t.Fatalf("request changed: %v", d.Data)
	}
}

func TestPeopleAreFoundByUsernameEmailOrName(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	for _, who := range []string{"staff2", "STAFF2", "staff2@example.test", "Thandi Teacher", " thandi teacher "} {
		r := f.ok(hr, "person_balances", map[string]any{"person": who})
		if get(r.Data, "person", "username") != "staff2" {
			t.Errorf("%q -> %v", who, r.Data)
		}
	}
	f.refused(hr, "person_balances", map[string]any{"person": "ghost"}, `No one matches "ghost"`)
	f.refused(hr, "person_balances", map[string]any{"person": "  "}, "Say who")

	// Two people with the same name are never guessed between.
	if _, err := f.db.Exec(`UPDATE users SET name = 'Thandi Teacher' WHERE username = 'staff1'`); err != nil {
		t.Fatal(err)
	}
	f.refused(hr, "person_balances", map[string]any{"person": "Thandi Teacher"}, "matches more than one person: Thandi Teacher (staff1)")
	f.ok(hr, "person_balances", map[string]any{"person": "staff1"})
}

func TestRecordAndRemoveLeave(t *testing.T) {
	f := newFixture(t)
	hr, staff := f.as("hr"), f.as("staff1")
	args := map[string]any{"person": "staff1@example.test", "type": "annual", "start": "2026-10-01", "end": "2026-10-02", "note": "Agreed by phone"}
	r := f.ok(hr, "record_leave", args)
	if get(r.Data, "request", "status") != "approved" || get(r.Data, "request", "recorded_by") != "Hannah HR" ||
		get(r.Data, "request", "person") != "Sam Staff" || get(r.Data, "request", "days") != float64(2) ||
		r.Data["below_zero"] != false || get(r.Data, "balance", "taken") != float64(2) || get(r.Data, "balance", "available") != float64(13) {
		t.Fatalf("record_leave = %v", r.Data)
	}
	// It is real leave: it counts against the balance and overlaps are refused.
	f.refused(hr, "record_leave", args, "Start date:")

	// The person cannot remove it; an admin can, and the days come back.
	f.refused(staff, "remove_recorded_leave", map[string]any{"id": id(r)}, "needs a leave administrator")
	f.refused(hr, "remove_recorded_leave", map[string]any{"id": 99999}, "No such request")
	rm := f.ok(hr, "remove_recorded_leave", map[string]any{"id": id(r)})
	if get(rm.Data, "request", "status") != "cancelled" {
		t.Fatalf("remove = %v", rm.Data)
	}
	if b := f.ok(hr, "person_balances", map[string]any{"person": "staff1"}); !strings.Contains(b.Text, `"taken":0`) {
		t.Fatalf("balances after removal: %s", b.Text)
	}
	f.refused(hr, "remove_recorded_leave", map[string]any{"id": id(r)}, "That recorded leave was already removed.")
	if g := f.ok(hr, "get_request", map[string]any{"id": id(r)}); get(g.Data, "request", "removed_by") != "Hannah HR" ||
		get(g.Data, "request", "removed_at") == nil || !strings.Contains(rm.Text, "no longer counts against their balance") {
		t.Fatalf("removal audit = %v / %s", g.Data, rm.Text)
	}

	// Ordinary approved leave is not removable this way.
	n := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12"})
	f.refused(hr, "remove_recorded_leave", map[string]any{"id": id(n)}, "Only leave an administrator recorded")
}

func TestRecordLeaveRules(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	base := func() map[string]any {
		return map[string]any{"person": "staff1", "type": "annual", "start": "2026-10-01", "note": "why"}
	}
	with := func(k string, v any) map[string]any { m := base(); m[k] = v; return m }

	f.refused(hr, "record_leave", with("note", "  "), "Note:")
	// The last day must be over: tomorrow is refused, today is fine, and so is
	// leave that began earlier and ends today.
	f.refused(hr, "record_leave", with("start", "2026-10-20"), "End date: Only leave that has already happened")
	f.refused(hr, "record_leave", map[string]any{"person": "staff1", "type": "annual", "start": "2026-10-05", "end": "2026-10-06", "note": "why"}, "End date: Only leave that has already happened")
	f.ok(hr, "record_leave", map[string]any{"person": "staff1", "type": "annual", "start": "2026-10-05", "note": "today"})
	f.refused(hr, "record_leave", with("start", "soon"), "Start date:")
	f.refused(hr, "record_leave", with("type", "nonsense"), "annual (Annual leave)")
	f.refused(hr, "record_leave", with("person", "ghost"), "No one matches")
	// A missing required argument is the SDK's own validation.
	m := base()
	delete(m, "note")
	f.refused(hr, "record_leave", m, "validating")
	// Nobody records their own leave.
	f.refused(hr, "record_leave", with("person", "hr"), "Person: Another admin records your own leave")
	f.makeAdmin("headkids")
	f.ok(f.as("headkids"), "record_leave", with("person", "hr"))

	// Going below zero is allowed and flagged: 15 days a year, 20 working days recorded.
	r := f.ok(hr, "record_leave", map[string]any{"person": "staff2", "type": "annual", "start": "2026-09-07", "end": "2026-10-02", "note": "Long break"})
	if r.Data["below_zero"] != true || get(r.Data, "request", "days").(float64) < 16 || !strings.Contains(r.Text, "below zero") {
		t.Fatalf("record_leave = %v", r.Data)
	}
	// A retired type can still be recorded, though a person cannot apply for it.
	if _, err := f.db.Exec(`UPDATE leave_types SET active = 0 WHERE code = 'annual'`); err != nil {
		t.Fatal(err)
	}
	f.refused(f.as("staff1"), "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12"}, "is not one of")
	old := f.ok(hr, "record_leave", map[string]any{"person": "staff1", "type": "annual", "start": "2026-10-01", "note": "From before it was retired"})
	if get(old.Data, "request", "status") != "approved" || old.Data["balance"] == nil {
		t.Fatalf("retired type = %v", old.Data)
	}
}

func TestAdjustBalanceAndPersonBalances(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	r := f.ok(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "days": 3, "reason": "Carry-over"})
	if get(r.Data, "balance", "adjustments") != float64(3) || get(r.Data, "balance", "available") != float64(18) ||
		get(r.Data, "balance", "cycle_start") != "2026-01-01" {
		t.Fatalf("adjust_balance = %v", r.Data)
	}
	f.ok(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "cycle_start": "2026-01-01", "days": -1, "reason": "Correction"})
	pb := f.ok(hr, "person_balances", map[string]any{"person": "staff1"})
	adj := pb.Data["adjustments"].([]any)
	if len(adj) != 2 || get(adj[0].(map[string]any), "by") != "Hannah HR" || len(pb.Data["balances"].([]any)) < 3 {
		t.Fatalf("person_balances = %v", pb.Data)
	}
	// The person sees the same through my_balances.
	if b := f.ok(f.as("staff1"), "my_balances", nil); !strings.Contains(b.Text, `"adjustments":2`) {
		t.Fatalf("my_balances = %s", b.Text)
	}

	f.refused(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "days": 0, "reason": "x"}, "Days: Enter a number")
	f.refused(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "days": 1, "reason": " "}, "Reason: Say why")
	f.refused(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "days": 1, "reason": "x", "cycle_start": "2026-03-01"}, "Cycle: That is not the start")
	f.refused(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "annual", "days": 1, "reason": "x", "cycle_start": "later"}, "Cycle: Choose a cycle")
	f.refused(hr, "adjust_balance", map[string]any{"person": "nobody", "type": "annual", "days": 1, "reason": "x"}, "No one matches")
	f.refused(hr, "adjust_balance", map[string]any{"person": "staff1", "type": "nonsense", "days": 1, "reason": "x"}, "Leave type")
}

func TestNegativeBalancePermissions(t *testing.T) {
	f := newFixture(t)
	hr, staff := f.as("hr"), f.as("staff1")
	ask := func(user *mcp.ClientSession) result {
		return f.ok(user, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-11-02", "end": "2027-01-29", "note": "trip", "ask_hr": true})
	}
	a, b := ask(staff), ask(f.as("staff2"))
	if q := f.ok(hr, "permission_queue", nil); len(q.Data["queue"].([]any)) != 2 ||
		get(q.Data["queue"].([]any)[0].(map[string]any), "shortfall_days").(float64) <= 0 {
		t.Fatalf("permission_queue = %v", q.Data)
	}

	g := f.ok(hr, "grant_negative", map[string]any{"id": id(a)})
	if get(g.Data, "request", "status") != "draft" || !strings.Contains(g.Text, "emailed") {
		t.Fatalf("grant = %v", g.Data)
	}
	if st := f.ok(staff, "get_request", map[string]any{"id": id(a)}); st.Data["can_submit"] != true {
		t.Fatalf("not submittable: %v", st.Data)
	}
	// A refusal needs a reason.
	f.refused(hr, "refuse_negative", map[string]any{"id": id(b), "note": " "}, "Note: Say why")
	rf := f.ok(hr, "refuse_negative", map[string]any{"id": id(b), "note": "Take it unpaid"})
	if get(rf.Data, "request", "status") != "declined" || get(rf.Data, "request", "decision_note") != "Take it unpaid" {
		t.Fatalf("refuse = %v", rf.Data)
	}
	if q := f.ok(hr, "permission_queue", nil); len(q.Data["queue"].([]any)) != 0 {
		t.Fatalf("queue should be empty: %v", q.Data)
	}
	// Both are decided now.
	f.refused(hr, "grant_negative", map[string]any{"id": id(b)}, "That is not possible")
	f.refused(hr, "refuse_negative", map[string]any{"id": id(a), "note": "x"}, "That is not possible")
	f.refused(hr, "grant_negative", map[string]any{"id": 99999}, "No such request")
	f.refused(hr, "refuse_negative", map[string]any{"id": 99999, "note": "x"}, "No such request")
}

func TestNobodyGrantsOrRefusesTheirOwnPermission(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	r := f.ok(hr, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-11-02", "end": "2027-01-29", "note": "trip", "ask_hr": true})
	f.refused(hr, "grant_negative", map[string]any{"id": id(r)}, "That is not possible")
	f.refused(hr, "refuse_negative", map[string]any{"id": id(r), "note": "no"}, "That is not possible")
	// Another administrator can.
	f.makeAdmin("headkids")
	f.ok(f.as("headkids"), "grant_negative", map[string]any{"id": id(r), "note": "fine"})
}

func TestReassignRequest(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	r := f.ok(f.as("staff1"), "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12"})
	for _, who := range []string{"headkids", "headkids@example.test", "Kagiso Kids"} {
		m := f.ok(hr, "reassign_request", map[string]any{"id": id(r), "approver": who})
		if get(m.Data, "request", "approver") != "Kagiso Kids" || !strings.Contains(m.Text, "Kagiso Kids") {
			t.Fatalf("reassign = %v", m.Data)
		}
	}
	// The new approver can now decide it.
	if q := f.ok(f.as("headkids"), "approval_queue", nil); len(q.Data["queue"].([]any)) != 1 {
		t.Fatalf("queue = %v", q.Data)
	}
	// A blank approver hands it to the administrators.
	m := f.ok(hr, "reassign_request", map[string]any{"id": id(r)})
	if get(m.Data, "request", "approver") != nil || !strings.Contains(m.Text, "administrators") {
		t.Fatalf("reassign to admins = %v", m.Data)
	}

	f.refused(hr, "reassign_request", map[string]any{"id": id(r), "approver": "ghost"}, "No one matches")
	f.refused(hr, "reassign_request", map[string]any{"id": 99999, "approver": "headkids"}, "No such request")
	// Not to the requester themselves: the service refuses.
	f.refused(hr, "reassign_request", map[string]any{"id": id(r), "approver": "staff1"}, "Approver: Not the person whose request it is.")
	// Only pending requests move.
	f.ok(hr, "decide_request", map[string]any{"id": id(r), "decision": "decline", "note": "no"})
	f.refused(hr, "reassign_request", map[string]any{"id": id(r), "approver": "headkids"}, "That is not possible")
	// Nor to someone who cannot sign in.
	r2 := f.ok(f.as("staff1"), "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-19"})
	if _, err := f.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'staff2'`); err != nil {
		t.Fatal(err)
	}
	f.refused(hr, "reassign_request", map[string]any{"id": id(r2), "approver": "staff2"}, "Approver: Choose someone who can currently sign in")
}

// A sign-in normalises "user@domain" and "DOMAIN\user" to the username, but a
// person named by email must be the one with that email, never a username
// that merely shares its local part.
func TestPersonLookupNeverMistakesAnEmailForAUsername(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	if _, err := f.db.Exec(`UPDATE users SET email = 'staff2@elsewhere.org' WHERE username = 'staff1'`); err != nil {
		t.Fatal(err)
	}
	if r := f.ok(hr, "person_balances", map[string]any{"person": "staff2@elsewhere.org"}); get(r.Data, "person", "username") != "staff1" {
		t.Fatalf("email -> %v", r.Data)
	}
	// Someone else's local part with a domain nobody has is no one.
	f.refused(hr, "person_balances", map[string]any{"person": "staff2@nowhere.org"}, "No one matches")
	f.refused(hr, "person_balances", map[string]any{"person": `EXAMPLE\staff2`}, "No one matches")
	f.refused(hr, "record_leave", map[string]any{"person": "hr@example.test.evil", "type": "annual", "start": "2026-10-01", "note": "x"}, "No one matches")

	// A username that is also another person's name is ambiguous, and the
	// list says who is who.
	if _, err := f.db.Exec(`UPDATE users SET name = 'staff2', disabled = 1 WHERE username = 'staff1'`); err != nil {
		t.Fatal(err)
	}
	f.refused(hr, "person_balances", map[string]any{"person": "staff2"}, "staff2 (staff1, disabled)")
	f.refused(hr, "person_balances", map[string]any{"person": "staff2"}, "Thandi Teacher (staff2)")
	// The disabled person still answers to what is unambiguous, with the flag shown.
	if r := f.ok(hr, "person_balances", map[string]any{"person": "staff1"}); get(r.Data, "person", "disabled") != true {
		t.Fatalf("disabled flag = %v", r.Data)
	}
}

func TestNobodyAdjustsOrUnrecordsTheirOwn(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	f.refused(hr, "adjust_balance", map[string]any{"person": "hr", "type": "annual", "days": 1, "reason": "x"}, "Person: Another admin adjusts your own balance.")
	f.makeAdmin("headkids")
	other := f.as("headkids")
	f.ok(other, "adjust_balance", map[string]any{"person": "hr", "type": "annual", "days": 1, "reason": "x"})

	// Leave recorded for an admin is removed by another admin, not by them.
	r := f.ok(other, "record_leave", map[string]any{"person": "hr", "type": "annual", "start": "2026-10-01", "note": "x"})
	f.refused(hr, "remove_recorded_leave", map[string]any{"id": id(r)}, "Person: Another admin removes leave recorded for you.")
	f.ok(other, "remove_recorded_leave", map[string]any{"id": id(r)})
}
