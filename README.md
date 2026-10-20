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
3. `FINAL_APPROVER`, for anyone with no usable manager;
4. the admins, where `FINAL_APPROVER`'s own leave goes.

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

Pending days are reserved, so the same days can't be requested twice. Working days skip
weekends and SA public holidays. The holidays are computed from the Public Holidays Act,
including the Sunday-to-Monday rule. Admins add proclaimed days, such as election days,
by hand. Days count against the cycle the leave starts in.

**Cancelling approved leave** is itself a request, approved by the same chain. The
leave stays booked until the cancellation is approved. Pending requests can simply be
withdrawn.

**Email and calendar** go through an outbox. Each is a database row written in the
same transaction as the change that causes it. A background worker then sends it,
retrying for about a day. Graph being down never blocks an approval, and nothing
is lost. Anything that still hasn't gone through shows on **Admin → Outbox** with a
Retry button.

## Local development

```sh
make run                 # http://localhost:8082 -- the fake directory, any password
make mailpit             # in another terminal: mail viewer at http://localhost:8025
make run-mail            # as run, but sends mail to mailpit
make check               # gofmt + vet + every test, uncached
make recreate-db         # drop the local database (after a schema change)
```

The fake directory is `testdata/directory.json`: `pastor` (the final approver),
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
| Dependencies | `go-ldap/ldap` (LDAP), `mattn/go-sqlite3` (cgo), `justinas/nosurf` (CSRF), `x/time/rate` (limits), `17xande-dev/mailer` (Graph mail and tokens). The Graph calendar client is about 150 lines here rather than the Graph SDK. |

### Decisions still open

| Open question | Current behaviour | What would decide it |
|---|---|---|
| **Entra ID sign-in (OIDC)** | LDAP only | Confirm AD syncs to Entra (Entra Connect). `directory.Directory` is the seam, and `objectGUID` = `onPremisesImmutableId` keeps everyone's history |
| Leave cycles per employee start date | Calendar year (sick: 36 months from `SICK_CYCLE_START_YEAR`) | HR needing BCEA cycles from each start date; AD would need a start-date attribute |
| Accrual (1.25 days/month) vs. full allowance up front | Up front | HR policy |
| Cancelling leave that has already started | Not possible in-app; an admin adjusts the balance | How often it happens |
| Delegating approvals while an approver is on leave | An admin reassigns | Heads asking for it |
| Half days | Not supported (by decision) | Revisit on request |
| Sign-in timing | An unknown username answers slightly faster than a wrong password (no DC bind), which reveals whether a username exists | Usernames are not secret inside the organisation; revisit if that changes |
