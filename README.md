# Leave

Leave requests for an organisation's staff. People sign in with their Windows (AD) credentials,
apply for leave, and their department head approves it. Heads' own leave goes to the
person above them. Every step sends an email, and approved leave appears on a shared
Microsoft 365 calendar.

- **Go**, standard library `net/http` and `html/template`, server-rendered.
- **htmx** only for the live day-count preview on the apply form. Every form is a plain
  POST that works without JavaScript.
- **SQLite** in a single file. **One plain CSS file**, no framework or build step.
- One binary with everything embedded, running in Docker on-prem next to the DC.

## How it works

**Who approves whom.** Each person's approver is resolved when they submit, in this order:

1. an admin's override for that person;
2. their `manager` in AD (a department head's manager is the senior person above them);
3. the **final approver** (set on **Admin → Settings**), for anyone with no usable manager;
4. the admins, where the final approver's own leave goes, and everyone's who has no
   manager while no final approver is set (or theirs is disabled).

A disabled account or yourself is always skipped. The approver is frozen on the
request, so a directory change afterwards cannot strand it. An admin can reassign it.

**Balances.** The BCEA defaults are seeded on first start and editable afterwards:

| Type | Allowance |
|---|---|
| Annual | 15 working days / year |
| Sick | 30 working days / 36-month cycle (may be backdated) |
| Family responsibility | 3 days |
| Maternity | 120 calendar days |
| Parental | 10 days |
| Unpaid | no limit |
| Floating holidays | set by an admin (seeded off, 0 days); never on the restricted day |
| Weekends off | set by an admin (seeded off, 0 weekends) |
| Company closure | no limit; used only by shutdowns |

Pending days are reserved, so the same days can't be requested twice. Leave costs only
**working days** (Mon–Fri by default, set on **Admin → Settings**) that aren't SA public
holidays. With Tuesday to Friday and Sunday ticked, for example, a Monday is free and a
Sunday costs a day. The holidays are computed from the Public Holidays Act,
including the Sunday-to-Monday rule. Admins add proclaimed days, such as election days,
by hand. Days count against the cycle the leave starts in.

**The restricted day.** The restricted day (e.g. Sunday, chosen on **Admin → Settings**,
none by default) is a working day nobody can take off by itself. Leave that includes it
must cover at least the minimum number of working days (3 by default), counting that day.
Each leave type sets its own rule on
**Admin → Leave types**:
- annual and unpaid leave: only within longer leave;
- sick, family, maternity and parental leave: no rule, since none of them can be planned;
- floating holidays: never.

**Weekends off** are the way to have the restricted day off by itself. A weekend off
runs Saturday to Sunday and costs one weekend from the yearly allowance. An admin can
give someone **long weekends** (on their page under **Admin → People**), which run
Friday to Sunday for the same cost.

**Company shutdowns** (**Admin → Shutdowns**) cover a period the whole organisation
closes. A shutdown is made of parts, each charged to one leave type: for example a week
to annual leave and the rest to *Company closure*, which deducts nothing. Everyone gets
a pre-filled draft per part and an email asking them to submit it. The dates and types
are fixed. A submitted request goes through the normal approval chain, or is approved
straight away if the shutdown says so. The days are mandatory, so they may take a balance
below zero, and they can't be withdrawn or cancelled. "Create requests for anyone
missing" covers new starters, and a shutdown can be deleted until someone submits.

**Coverage limits** (**Admin → Coverage limits**) cap how many people of one department
may be away on a single day, as a percentage of the department, with a default for
departments that have no limit of their own. The first person away is always allowed.
A request over the limit is refused unless the person asks for **special permission**.
It is then approved level by level, each approver passing it to their own approver, up
to the final approver, and the request page shows the trail. Approvers see the warning as
the department stands now. Only leave types marked as blockable are refused; the others
still count as being away.

**Negative leave.** Someone whose request would take a balance below zero can ask HR
for permission instead of being refused, and must say why. The request waits on
**Admin → Permissions** and reserves nothing. An admin grants or refuses it with a
note, and the person is emailed. A granted request comes back as a draft for them to
submit, and the usual rules (including the coverage limit) apply again then.

**Cancelling approved leave** is itself a request, approved by the same chain. The
leave stays booked until the cancellation is approved. Pending requests can simply be
withdrawn.

**Settings** (**Admin → Settings**) hold the policy an admin may change without a
deploy, and each change applies to the next request: the working days, the restricted
day and its minimum, the final approver, how far back sick leave may start, and the year
the 36-month sick cycle counts from. They live in the database; the environment keeps
only wiring and secrets.

**The admin area** has: All requests (with a CSV export), People (approver overrides,
balance adjustments, long weekends), Leave types, Public holidays, Shutdowns, Coverage
limits, Permissions, Settings and Outbox.

**Email and calendar** go through an outbox. Each is a database row written in the
same transaction as the change that causes it. A background worker then sends it,
retrying for about a day. Graph being down never blocks an approval, and nothing
is lost. Anything that still hasn't gone through shows on **Admin → Outbox** with a
Retry button.

## AI assistants (MCP)

The app is also an [MCP](https://modelcontextprotocol.io) server, so people can ask an AI
assistant about their leave, apply for it, or approve what is waiting for them. Add a
custom connector in claude.ai, Claude Desktop or Claude Code with the URL

    BASE_URL/mcp          e.g. https://leave.example.org/mcp

**How people connect.** Adding the connector sends them to the app's normal sign-in page
(their Windows credentials), then to a consent screen naming the assistant. **Allow**
connects it, **Deny** does not. **Connected apps** (the *Apps* link in the navigation,
`/apps`) lists each connection with when it was made and last used, and **Disconnect**
ends it at once. Nothing else is needed from an administrator.

**What an assistant can do** is exactly what the person can do themselves. Every tool
runs as them through the same code the pages use (the same service methods and
the same permission checks), so there is no second set of rules. A refusal comes back to the assistant as a
message it can relay, not an error. Tools that change anything are marked as such, so
clients ask the person before calling them.

| Tool | What it does |
|---|---|
| `my_balances` | Balances per leave type for the current cycle |
| `my_requests` | Their own requests, optionally one status |
| `get_request` | One request in full: decision trail, coverage warning, which actions are open to them |
| `team_leave` | Current and upcoming leave of the people who report to them |
| `public_holidays` | Holidays for a year |
| `preview_request` | Day count, balance afterwards and every rule that would stop a request (dates, restricted day, overlap, coverage, balance), and whether special permission or asking HR is available. Changes nothing |
| `apply_for_leave` | Submit a request. `override` asks for special permission and `ask_hr` asks HR to allow a negative balance, like the two buttons on the form |
| `withdraw_request` | Withdraw their own undecided request |
| `submit_draft` | Submit a shutdown draft, or one HR allowed to go below zero |
| `request_cancellation` | Ask for approved, not-yet-started leave to be cancelled |
| `approval_queue` | What is waiting for their decision, with coverage warnings, who it goes to next, and the trail |
| `decide_request` | Approve or decline (a decline needs a note) |
| `all_requests` | Administrators only: every request, filtered by status, dates and person |
| `who_is_away` | Administrators only: who is away in a date range, by department |
| `outbox_problems` | Administrators only: failing emails and calendar jobs |

**How it works underneath.** `/mcp` is a Streamable HTTP endpoint (stateless, JSON
responses) in the same binary, using the official Go SDK. The app is its own OAuth 2.1
authorization server for it: authorization code with PKCE (S256 only), dynamic client
registration for public clients (what claude.ai uses), refresh token rotation and
revocation, with the discovery documents at `/.well-known/oauth-protected-resource` and
`/.well-known/oauth-authorization-server`. Access tokens are opaque, last an hour and
are stored as sha256, like sessions. Refresh tokens last 30 days and rotate on every use;
presenting an old one again ends the whole connection. A token is checked against the
user on every call, so disabling someone in the directory stops their assistants at
once. Registration is open to anyone, so the consent screen says plainly that the
assistant chose its own name. Redirect URIs must match exactly, and are `https`, a
loopback `http` address, or an app's own scheme. `/mcp`, `/oauth/token`,
`/oauth/register`, `/oauth/revoke` and the metadata carry no cookies and are exempt from the CSRF
check (the list is pinned by a test); the consent form at `/oauth/authorize` keeps it.
`BASE_URL` must be set to the public address: it is the OAuth issuer.

**Behind a reverse proxy.** The SDK refuses a request that *arrives on a loopback address*
with a non-loopback `Host` header, to defeat DNS rebinding. The check looks at the
address the connection was accepted on, not where the proxy is. The documented
deployment is safe: the container listens on its own network address (the published
`127.0.0.1:8082` is the host side) and gets the public `Host`. It breaks if the binary
instead listens on loopback and the proxy forwards the public `Host` to it, for example
a proxy on the same machine running the binary outside Docker. Every `/mcp` call then
gets `403 Forbidden: invalid Host header`. Fix it by listening on a non-loopback
address (`LISTEN_ADDR=:8080`), not by turning the check off. A test pins the
behaviour so a change in the SDK is noticed.

## Local development

```sh
make run                 # http://localhost:8082 -- the fake directory, any password
make mailpit             # in another terminal: mail viewer at http://localhost:8025
make run-mail            # as run, but sends mail to mailpit
make check               # gofmt + vet + every test, uncached
make recreate-db         # drop the local database (after a schema change)
```

The fake directory is `testdata/directory.json`: `pastor` (set as the final approver on **Admin → Settings**, which starts with none),
`headops` and `headkids` (department heads), `staff1`, `staff2`, and `hr` (an admin). Any
non-empty password works. The config refuses `DIRECTORY=fake` unless
`COOKIE_SECURE=false`, so it cannot reach a real deployment. Without Graph configured,
calendar events are logged instead of created.

Opt-in tests against the real services:

```sh
set -a; source .env; set +a
LDAP_LIVE=1 LDAP_LIVE_USER=you LDAP_LIVE_PASSWORD="$(read -rs p; echo $p)" \
  go test -run Live ./internal/directory/
GRAPH_LIVE=1 go test -run Live ./internal/calendar/   # creates and deletes one event in 2030
```

## Deployment

### 1. Active Directory

- Create a read-only service account (`LDAP_BIND_DN`). Plain domain-user rights are enough
  to read people and group membership.
- Create the admin group (`LDAP_ADMIN_GROUP`). Nested groups work.
- Make sure each person's **Manager** field (Organization tab) is filled in. That is the
  approval chain. The admin's People page shows each person's resolved approver, so gaps
  are easy to spot.
- The DC must offer **LDAPS (636)**, or StartTLS on 389. The app refuses to send
  passwords in the clear. If the DC's certificate comes from an internal CA, mount its
  PEM and set `LDAP_CA_FILE`.
- Keep AD's account-lockout threshold above 5 attempts. The app's own sign-in limit
  (5 per username per ~15 minutes) is set lower, so a guesser hits the app's limit
  before a colleague is locked out of Windows.

### 2. Microsoft Entra app registration

Reuse an existing registration or create one. Under **API permissions → Microsoft
Graph → Application permissions**, add `Mail.Send` and `Calendars.ReadWrite`, then
**grant admin consent**. Both permissions apply tenant-wide until restricted, so limit
them to the two mailboxes with Exchange Online PowerShell:

```powershell
New-DistributionGroup -Name "Leave app mailboxes" -Type Security -Members leave@example.org
New-ApplicationAccessPolicy -AppId <client-id> -PolicyScopeGroupId "Leave app mailboxes" `
  -AccessRight RestrictAccess -Description "Leave: mail + calendar"
Test-ApplicationAccessPolicy -AppId <client-id> -Identity someone.else@example.org  # expect Denied
```

Create a client secret and put it in `.env`. It expires, so set a calendar reminder to
renew it. When it lapses, every email and calendar update piles up on the Outbox page
until the new secret is in place, then Retry clears them.

### 3. The server

```sh
make publish                          # on a workstation: build + push ghcr.io/17xande-dev/leave:<sha>
# on the server, next to compose.yaml:
cp .env.example .env && $EDITOR .env  # set TAG to the pushed sha
docker compose pull && docker compose up -d
docker compose logs -f
```

The database lives in the `leave-data` volume. Back it up with
`docker compose exec leave sqlite3 /data/leave.db ".backup /data/backup.db"`. That uses
SQLite's backup API, because a plain `cp` of a live database can capture a torn state.

The container listens on 8080, published on 127.0.0.1:8082 (HOST_PORT) behind the TLS-terminating reverse proxy. Set
`TRUSTED_PROXIES` to the proxy's address so sign-in rate limits see the real client IP.

## Decisions

| Decision | Choice, and why |
|---|---|
| Sign-in | LDAP bind against the DC, per request, with no connection pool, so one user's bind can never leak into another's request. People are keyed on `objectGUID`, which survives OU moves and renames. |
| Database | SQLite: one instance, one file. Switch to Postgres if a second instance or an outside reader is ever needed. |
| Styling | One plain CSS file. No Web Awesome, because a component framework and a CDN or vendoring step bought nothing here. |
| Dependencies | `go-ldap/ldap` (LDAP), `mattn/go-sqlite3` (cgo), `justinas/nosurf` (CSRF), `x/time/rate` (limits), `17xande-dev/mailer` (Graph mail and tokens), `modelcontextprotocol/go-sdk` (the MCP server: the protocol is still moving and tracking it by hand is a thin reimplementation for one person to maintain). The SDK brings `google/jsonschema-go`, `segmentio/encoding` (with `segmentio/asm`), `yosida95/uritemplate/v3` and `golang.org/x/oauth2`, and raises `x/sync` and `x/sys`. The OAuth server is about 600 lines on the standard library, and the Graph calendar client is about 150 lines here rather than the Graph SDK. |

### Decisions still open

| Open question | Current behaviour | What would decide it |
|---|---|---|
| **Entra ID sign-in (OIDC)** | LDAP only | Confirm AD syncs to Entra (Entra Connect). `directory.Directory` is the seam, and `objectGUID` = `onPremisesImmutableId` keeps everyone's history |
| Leave cycles per employee start date | Calendar year (sick: 36 months from the start year in Settings) | HR needing BCEA cycles from each start date; AD would need a start-date attribute |
| Accrual (1.25 days/month) vs. full allowance up front | Up front | HR policy |
| Cancelling leave that has already started | Not possible in-app; an admin adjusts the balance | How often it happens |
| Work patterns per person | One working week for everyone; a part-timer's non-working days are charged | Part-time staff or a second pattern, which would need a per-person week |
| Delegating approvals while an approver is on leave | An admin reassigns | Heads asking for it |
| Half days | Not supported (by decision) | Revisit on request |
| Sign-in timing | An unknown username answers slightly faster than a wrong password (no DC bind), which reveals whether a username exists | Usernames are not secret inside the organisation; revisit if that changes |
