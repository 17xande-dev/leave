# Leave

Leave requests for an organisation's staff. People sign in with their Windows (AD) credentials,
apply for leave, and their department head approves it. Heads' own leave goes to the
person above them. Every step sends an email, and approved leave appears on a shared
Microsoft 365 calendar.

- **Go**, standard library `net/http` and `html/template`, server-rendered.
- **htmx** only for the live preview on the apply form (days charged, and which days are not and why). Every form is a plain
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
| Sick | 30 working days / 36-month cycle |
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

Each type also sets how far back its leave may start (**Backdating (days)**, 0 to 92;
every type defaults to 92, about three months, the most the policy allows). 0 means it
cannot start in the past. The apply form's date picker greys out earlier days, and the
server refuses them whichever way a request arrives.

The picker also marks public holidays (a dot) and the person's own pending and approved
leave (hatching, lighter when pending), on the apply form and, for the person being
recorded for, on the admin Record leave form. The marks are advisory: they come from a
JSON data block (today minus 92 days to today plus a year) and never disable a day.

Under the day count the form says who else is away in the chosen dates ("Also away
then: Thandi (12–14 Mar), Sam (13 Mar, 20 Mar), and 2 more"): people in your department,
in a volunteer team you share, or who report to you, with pending (marked "not yet
approved") and approved leave. Each person appears once with all their dates, up to five
people, and "and N more" counts people. It shows names and dates only, never the type, so
sick leave stays private. It is information: it does not change the coverage limit.

**Approvals** (`/approvals`) lists what waits for you, oldest first. Each row says what is
left of the requester's balance for the request's own cycle once it is approved ("11
working days left in this cycle afterwards", "2 working days below zero afterwards";
nothing for unlimited types, and the cycle is named, "in 2027", when it is not the
current one). A pending request is already reserved against the balance, so this is the
balance as it stands now, not the balance minus the request again; when the person has
other pending requests of that type in that cycle they are counted too, and the row says
"if all their pending requests are approved" instead of "afterwards". Approving a
cancellation gives the days back. Each row also lists who else is away on those dates
("Also away: Thandi, Sick leave (12–14 Mar), ..."), from the *requester's* department,
teams and reports (not the approver's own), up to five people then "and N more", pending
marked, the requester left out. What you may see of each is still yours: the leave type
shows only for your reports, requests you are the approver of, or everyone if you are an
admin (`Service.AwayFor`). **Approve selected**
approves the ticked rows in one post, each through the normal approval; ticked rows that
fail (already decided, not yours) are listed on the page and the rest stay approved.
Rows needing special permission have no tick box: each needs its own "Override"
button, and the bulk post refuses them too (inside the decision's transaction). On a phone each
request is a block rather than a table row. At most 50 per post. **Decline...** on a row
opens a reason box (required) and declines from the queue; **Details** opens the
request. No JavaScript.

**Team** (`/team`, in the navigation for everyone) is a month grid of who is away: one row
per person, one column per day, for your department, the volunteer teams you share, and the
people who report to you, plus a row marked "You" for your own leave. Solid cells are
approved and hatched cells are awaiting approval (each also has hidden text for screen
readers); weekends and non-working days (**Admin → Settings**) and public holidays (named
on hover) are shaded, and today's column is marked. `?month=YYYY-MM` picks the month, with
Previous, Next and This month links; a bad value shows the current month, and the range is
12 months back to 24 forward. Everyone with leave in the month gets a row (no cap). Like
the apply hint, the grid never shows the leave type, whoever is looking; approvers still
get the existing list with types below it, "Leave of the people you approve for". No
JavaScript.

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

**Volunteer teams** add a second limit of the same kind: no more than n% of a team's
members may be away at once. Everyone can be in any number of teams. Teams come from
the directory (AD groups under `LDAP_TEAMS_BASE_DN`, synced hourly with the users), and
only people with an account here are counted, in both the team's size and who is away.
**Admin → Teams** lists them, each with an optional limit, plus a default (also on
**Admin → Settings**) for every team with none of its own. The warning names the team
("67% of the Band team (2 of 3) would be away..."), the same **special permission**
path applies, and a request over several limits (its department and one or more teams)
shows every warning. A team that disappears from the directory is disabled, not
deleted, so its limit survives a brief outage; it is hidden behind "Show inactive
teams". A person's teams are shown, read-only, on their admin page. With
`LDAP_TEAMS_BASE_DN` unset the feature is off.

**Negative leave.** Someone whose request would take a balance below zero can ask HR
for permission instead of being refused, and must say why. The request waits on
**Admin → Permissions** and reserves nothing. An admin grants or refuses it with a
note, and the person is emailed. A granted request comes back as a draft for them to
submit, and the usual rules (including the coverage limit) apply again then.

**Cancelling approved leave** is itself a request, approved by the same chain. The
leave stays booked until the cancellation is approved. Pending requests can simply be
withdrawn.

**Editing a pending request.** The person who made a request can change its type, dates
and note instead of withdrawing it and applying again (**Edit this request** on its page,
or `edit_request` over MCP), until anyone has started to decide it: it must still be
pending, ordinary leave (not a shutdown request, not recorded leave) and have no decision
steps. Anything else is refused with "withdraw it and apply again". The edit runs the same
checks as a new application, with the request's own old version left out of the overlap,
coverage and balance sums. An edit that is over a coverage limit is refused unless the
person asks for special permission, as for a new request, and one that is no longer over a
limit stops needing it. An edit never asks HR to go below zero: withdraw and apply again.
The request stays with the approver it was sent to and keeps its place in the queue; that
approver (or the admins, for the admins' queue) is emailed what changed, and the person is
not. An edit that changes nothing sends nothing. Every decision form posts a fingerprint of what the approver saw (type, dates, days, special-permission flag, note); a request edited since is not approved or declined, and the approver is sent back to look again. Bulk approval ticks carry it too, per row. There is no "edited on" mark: it would
need a new column.

**Settings** (**Admin → Settings**) hold the policy an admin may change without a
deploy, and each change applies to the next request: the working days, the restricted
day and its minimum, the final approver, and the year the 36-month sick cycle counts
from. They live in the database; the environment keeps
only wiring and secrets.

**Record leave** (on a person's page under **Admin → People**) writes down leave someone
has already taken, such as leave from before the system went live or agreed off it. Only
leave that has already happened can be recorded: its last day must be today or earlier,
and future days go through an application so approval and coverage cannot be bypassed.
It is approved at once, with the admin as approver, and counts like any request
(history, team view, overlap checks, the calendar). What is relaxed is what makes no
sense after the fact: the backdating limit, department and team coverage limits, the
restricted-day rule, and a leave type since retired. The balance may go below zero; the
request is flagged and the form says so. A note saying why is required, and it still
refuses overlap with the person's other leave. The person is emailed. Nobody records their
own leave: another admin does. The request page shows who recorded it, and **Remove this
recorded leave** (another admin, never for oneself) cancels it, so it no longer counts
against the balance, deletes the calendar event, emails the person, and keeps a
**Removed by** line with who and when. Ordinary approved leave cannot be removed this
way. Nobody adjusts their own balance either.

**The admin area** has: All requests (with a CSV export), People (a search box, each
person's annual leave available, approver overrides, balance adjustments, long weekends,
their teams), Leave types, Public holidays,
Shutdowns, Coverage limits, Teams, Permissions, Settings and Outbox.

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
message it can relay, not an error. Every tool carries an annotation: read-only, additive
(it changes something without destroying anything, such as applying for leave or recording
it) or destructive (it cannot be undone or decides something for someone else: withdraw,
decide, remove recorded leave, grant, refuse, reassign). Whether a client asks the
person before calling a tool is the client's choice; destructive ones are the ones
they usually ask about. The server's instructions also tell the assistant never to change
anything unless the person has asked.

| Tool | What it does |
|---|---|
| `my_balances` | Balances per leave type for the current cycle |
| `my_requests` | Their own requests, optionally one status |
| `get_request` | One request in full: decision trail, coverage warning, which actions are open to them |
| `team_leave` | Current and upcoming leave of the people who report to them |
| `public_holidays` | Holidays for a year |
| `preview_request` | Day count, which days in the range are not charged and why, balance afterwards and every rule that would stop a request (dates, restricted day, overlap, coverage, balance), and whether special permission or asking HR is available. Changes nothing |
| `apply_for_leave` | Submit a request. `override` asks for special permission and `ask_hr` asks HR to allow a negative balance, like the two buttons on the form |
| `edit_request` | Change the type, dates or note of their own pending request that nobody has started to decide (`can_edit` on `get_request` says whether it can be). Same checks as `apply_for_leave`, with the request's old version left out of overlap, coverage and balance; `override` as there; no `ask_hr` (a balance problem is refused: withdraw and ask HR). The approver is emailed what changed |
| `withdraw_request` | Withdraw their own undecided request |
| `submit_draft` | Submit a shutdown draft, or one HR allowed to go below zero |
| `request_cancellation` | Ask for approved, not-yet-started leave to be cancelled |
| `approval_queue` | What is waiting for their decision, with coverage warnings, who it goes to next, the requester's balance afterwards (`balance_after`, absent for unlimited types) and the trail |
| `decide_request` | Approve or decline (a decline needs a note). `expected` is optional: pass the `fingerprint` from `get_request` or `approval_queue` and the decision is refused if the person has edited the request since |
| `all_requests` | Administrators only: every request, filtered by status, dates and person |
| `who_is_away` | Administrators only: who is away in a date range, by department |
| `outbox_problems` | Administrators only: failing emails and calendar jobs |
| `person_balances` | Administrators only: a person's balances for the current cycle and every adjustment made for them |
| `permission_queue` | Administrators only: requests waiting for HR to allow a negative balance (`/admin/permissions`), with the shortfall |
| `record_leave` | Administrators only: record leave a person has already taken (the **Record leave** form): its last day must be today or earlier. A note is required; a retired type is allowed; the balance may go below zero and the result says so. The person is emailed |
| `remove_recorded_leave` | Administrators only: remove leave an administrator recorded; it no longer counts against their balance and the person is emailed |
| `adjust_balance` | Administrators only: add or remove days on one type's balance for a cycle (default the current one), with a required reason. Nobody is emailed |
| `grant_negative` | Administrators only (destructive: it cannot be recalled): allow a request awaiting HR to go below zero; it returns to the person as a draft and they are emailed. The note is optional |
| `refuse_negative` | Administrators only: decline a request awaiting HR; a note is required and the person is emailed |
| `reassign_request` | Administrators only: move a pending request to another approver, or with none to the leave administrators; the new approver is emailed |

The administrators' tools identify a person by sign-in username (as `all_requests` does),
email address or exact full name, and refuse a name that matches two people rather than
guess. They are the website's admin actions through the same service methods, so the same
rules hold: nobody records or removes leave recorded for themselves, adjusts their own
balance, or grants or refuses their own request (another administrator does), and a validation message from the service comes back as the tool's
error naming the field.

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

## Appearance

The pages follow the system's light or dark setting. The Auto, Light and Dark buttons
in the header override it with a `theme` cookie, set by `POST /theme` (no JavaScript,
which the CSP would block anyway); Auto clears it. The colours are custom properties
in the `:root` block of `internal/handler/static/styles.css`, each declared once with
`light-dark()`, and a test checks their contrast in both themes. Changing the theme reloads the page, so unsaved input in a form on it is lost. Emails stay light.

## Local development

```sh
make run                 # http://localhost:8082 -- the fake directory, any password
make mailpit             # in another terminal: mail viewer at http://localhost:8025
make run-mail            # as run, but sends mail to mailpit
make check               # gofmt + vet + every test, uncached
make recreate-db         # drop the local database (after a schema change)
```

The fake directory is `testdata/directory.json`: `pastor` (set as the final approver on **Admin → Settings**, which starts with none),
`headops` and `headkids` (department heads), `staff1`, `staff2`, and `hr` (an admin).
Each has a `"teams"` list: `staff1`, `hr` and `headops` are in the Band, `staff2` and
`headkids` in Kids team, so **Admin → Teams** has two working teams. Any
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
- Optional, for team limits: put one AD group per volunteer team in an OU and set
  `LDAP_TEAMS_BASE_DN` to it. Each group is a team (named by its `description` if it has
  one, else its `cn`), and its enabled members, nested groups included, are the people
  in it. The service account needs only read access. Left unset, there are no teams and
  **Admin → Teams** says so.
- Make sure each person's **Manager** field (Organization tab) is filled in. That is the
  approval chain. The admin's People page shows each person's resolved approver, so gaps
  are easy to spot.
- The DC must offer **LDAPS (636)**, or StartTLS on 389. The app refuses to send
  passwords in the clear. If the DC's certificate comes from an internal CA, copy the
  CA's PEM into `certs/` next to `compose.yaml` on the server (git-ignored, mounted
  read-only at `/certs`) and set `LDAP_CA_FILE=/certs/<file>.pem`. Use the DC's full
  name in `LDAP_URL`, because that is what its certificate names. Check the setup with
  the `LDAP_LIVE=1` test rather than `ldapsearch` or `openssl`: recent OpenSSL refuses
  a 1024-bit DC key that the app itself accepts.
- Keep AD's account-lockout threshold above what the app's own sign-in limit lets
  through, so a guesser hits the app's limit before a colleague is locked out of
  Windows. The limit is a token bucket per username: a burst of 5 attempts, then one
  more every 3 minutes. That is at most 6 attempts in 3 minutes and 10 in 15 minutes,
  and about 20 more per hour after that. Set AD's threshold above the count for the
  lockout observation window you use (a 15-minute window needs a threshold of 11 or
  more). A separate limit per client address (burst 20, one every 10 seconds) slows
  spraying many usernames from one place.

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

`make deploy` updates the server from a workstation. It clones or pulls the repo on the
server, then rebuilds and restarts the container with `docker compose up -d --build`. It
refuses to run with uncommitted or unpushed changes, because the server pulls from GitHub.

One-time setup:

1. **Name the server in `~/.ssh/config`** on your workstation. The address stays out of the
   repo, and the Makefile uses only the alias (`DEPLOY_HOST`, default `docker-server`):
   ```
   Host docker-server
     HostName <the server's address>
     User <your user on the server>
   ```
2. **Nothing to set up for GitHub.** The repo is public, so the server clones it over
   HTTPS (`REPO`) with no key. If the repo ever goes private again, switch `REPO` back to
   the `git@github.com:` form and give the server a read-only deploy key. Also run
   `ssh -T git@github.com` on the server once to accept GitHub's host key, or the clone
   fails with "Host key verification failed".
3. **Run `make deploy` once.** It clones into `~/leave` (`DEPLOY_DIR`) and stops, because
   there's no `.env` yet. On the server, run `cp ~/leave/.env.example ~/leave/.env` and fill
   it in, then deploy again.

After that, every deploy is: commit, push, `make deploy`. Logs are at
`ssh docker-server 'cd ~/leave && docker compose logs -f'`.

The schema has no migrations. If the app refuses to start against an old database, the
logs say so. Back up the database (below) while the old version is still running -- the
new one will not start against it, so `exec` will not work after the deploy -- then
recreate it with `docker compose down -v` and `make deploy`. `down -v` deletes the
`leave-data` volume and everything in it, so copy the backup out of the volume first.

`make publish` (build on a workstation, push to GHCR, `docker compose pull` on the server)
is still there for when you'd rather the server never build anything.

The database lives in the `leave-data` volume. Back it up and copy it out of the volume:

```sh
docker compose exec leave sqlite3 /data/leave.db ".backup /data/backup.db"
docker compose cp leave:/data/backup.db ./leave-backup-$(date +%F).db
```

`.backup` uses SQLite's backup API, because a plain `cp` of a live database can capture a
torn state.

The container listens on 8080 and is published only on `127.0.0.1:8082` (`HOST_PORT`), for
checking from the server itself. Public access goes through the tunnel below.

### 4. Public access: a Cloudflare Tunnel

The app must be served over HTTPS: its session cookies are `Secure`, and `BASE_URL`
(the public address) is used for CSRF checks, email links and the AI connector's OAuth
issuer. `compose.yaml` runs `cloudflared` next to the app. It makes an outbound-only
connection to Cloudflare, so no inbound port is opened, and Cloudflare provides the
certificate.

What has to exist (once):

1. **A remotely-managed tunnel** in the Cloudflare account that owns your domain, with a
   public hostname routing `leave.example.org` to the service `http://leave:8080`
   (the compose service name, on the compose network), and the DNS record for it:
   a proxied `CNAME` to `<tunnel-id>.cfargotunnel.com`. With the `cf` CLI:
   ```sh
   cf tunnels create --name leave --config-source cloudflare
   cf tunnels config update <tunnel-id> --body '{"config":{"ingress":[
     {"hostname":"leave.example.org","service":"http://leave:8080"},
     {"service":"http_status:404"}]}}'
   cf dns records create --zone <your domain> --body '{"type":"CNAME","name":"leave",
     "content":"<tunnel-id>.cfargotunnel.com","proxied":true,"ttl":1}'
   ```
   A tunnel and its DNS record must be in the same Cloudflare account.
2. **The tunnel token in `tunnel.env`** next to `compose.yaml` on the server, one line,
   `TUNNEL_TOKEN=...` (`cf tunnels token get <tunnel-id>`). It is a secret: the file is
   git-ignored, docker-ignored, and should be mode 600. It is separate from `.env` so
   it is not loaded into the app's own environment.
3. **`BASE_URL=https://leave.example.org` and `TRUSTED_PROXIES=172.30.16.10/32`** in
   `.env`. The second is the `cloudflared` container's fixed address on the compose
   network (the subnet is fixed in `compose.yaml`). The app trusts forwarding headers
   only from that one address, which is how the sign-in rate limit sees real client IPs
   instead of the tunnel's. If you change the subnet, change this to match.

Then `make deploy`. Check it with `docker compose logs cloudflared` (look for
"Registered tunnel connection") and open `BASE_URL`.

Note that this puts the AD sign-in page on the internet. The app rate-limits sign-in per
username and per address, but consider putting **Cloudflare Access** in front, which
restricts who can even reach the page. If you want the AI connector to keep working,
`/mcp`, `/oauth/*` and `/.well-known/*` must stay reachable without that login, so they
need a bypass rule.

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
| **Planning Center as the team source** | AD security groups. `directory.TeamSource` is the seam: a PCO source would give each team's id and match members to users by email | A read-only look at PCO showed 74 Services teams whose names repeat per campus and service type (Band at Sandton AM2, Kyalami, Centurion, Ballito), and limits would apply to some and not others, so the mapping from PCO teams to limits needs deciding first |
| Leave cycles per employee start date | Calendar year (sick: 36 months from the start year in Settings) | HR needing BCEA cycles from each start date; AD would need a start-date attribute |
| Accrual (1.25 days/month) vs. full allowance up front | Up front | HR policy |
| Cancelling leave that has already started | Not possible in-app; an admin adjusts the balance | How often it happens |
| Work patterns per person | One working week for everyone; a part-timer's non-working days are charged | Part-time staff or a second pattern, which would need a per-person week |
| Delegating approvals while an approver is on leave | An admin reassigns | Heads asking for it |
| Half days | Not supported (by decision) | Revisit on request |
| Sign-in timing | An unknown username answers slightly faster than a wrong password (no DC bind), which reveals whether a username exists | Usernames are not secret inside the organisation; revisit if that changes |
