# Leave

Leave requests for an organisation's staff. People sign in with their Windows (AD) credentials,
apply for leave, and their department head approves it. Heads' own leave goes to the
person above them. Every step sends an email, and approved leave appears on a shared
Microsoft 365 calendar.

- **Go**, standard library `net/http` and `html/template`, server-rendered.
- **htmx** only for the live preview on the apply form (days charged, and which days are not and why). Every form is a plain
  POST that works without JavaScript.
- **SQLite** in a single file. **One plain CSS file**, no framework or build step.
- One binary with everything embedded, running in Docker on the on-prem sdt-vm-docker02 host next to the DC, deployed with Kamal.

## How it works

**Who approves whom.** Each person's approver is resolved when they submit, in this order:

1. an admin's override for that person;
2. their `manager` in AD (a department head's manager is the senior person above them);
3. the **final approver** (set on **Admin → Settings**), for anyone with no usable manager;
4. the admins, where the final approver's own leave goes, and everyone's who has no
   manager while no final approver is set (or theirs is disabled).

A disabled account or yourself is always skipped. The approver is frozen on the
request, so a directory change afterwards cannot strand it. An admin can reassign a pending request, including their own, to another administrator (never to the person whose request it is) or to the leave administrators. An approver override on a person's page can likewise only name an administrator other than that person. Overrides set before this rule keep working until changed. An approver who is away can name a stand-in for a date range (**Approvals while you are away**, under "Away" in the navigation); the request stays theirs.

**Stand-ins while away.** Anyone can name a stand-in for the approvals that come to them, on **Away** or through the assistant tools; an admin can set one up or end one for anyone from the person's page (**Admin → People**) and sees them all at **Admin → Stand-ins**. For the dates chosen (the first day today or later, at most 30 days, both days counted) the stand-in also sees what waits for the approver in **Approvals** and may approve or decline it, as the approver would; the approver can still decide it too, and whatever is undecided when it ends is the approver's alone. Anyone who can sign in can be named, an approver has one stand-in at a time, and there are no chains: a stand-in acts for the person who named them, never for people who named them. A stand-in can never decide the leave of whoever set the delegation up (an admin who arranges cover for their own approver does not cover their own leave), and the leave administrators' queue is not delegated. The stand-in and, when an admin made it, the approver are emailed when it is set up and ended, and the stand-in is copied on what arrives for the approver meanwhile. A decision records who made it and for whom ("Approved by B for A") on the request, its history, the email and the calendar event. For an escalated request the stand-in's approval counts as the approver's: it moves up from the approver's level, and a stand-in for the final approver gives the final approval.

### Before deploying the reassignment rule

Run this read-only precheck on the host copy before deploying. Show the first list
to the operator; existing overrides keep working until changed. Expect the second
query to be empty. Pending requests already with non-admin managers are untouched;
past reassignments are not recorded and cannot be listed.

```sql
SELECT o.user_id, s.username AS subject, o.approver_id, a.username AS approver, a.is_admin, a.disabled,
       (o.approver_id = o.user_id) AS self
FROM approver_overrides o JOIN users s ON s.id = o.user_id JOIN users a ON a.id = o.approver_id
WHERE a.is_admin = 0 OR a.disabled = 1 OR o.approver_id = o.user_id ORDER BY s.username;
SELECT id, user_id, status FROM requests WHERE approver_id = user_id AND status IN ('pending', 'awaiting_hr', 'draft');
SELECT r.id, r.user_id, r.approver_id FROM requests r JOIN users a ON a.id = r.approver_id
WHERE r.status = 'pending' AND a.disabled = 1;
```

**Balances.** The BCEA defaults are seeded on first start and editable afterwards:

| Type | Allowance |
|---|---|
| Annual | 15 working days / year |
| Sick | 30 working days / 36-month cycle |
| Family responsibility | 3 days |
| Maternity | 120 calendar days |
| Parental | 10 days |
| Compassionate | no limit; not BCEA, a death in the family, at the employer's discretion (an admin may set an allowance) |
| Unpaid | no limit |
| Floating holidays | set by an admin (seeded off, 0 days); never on the restricted day |
| Weekends off | set by an admin (seeded off, 0 weekends) |
| Company closure | no limit; used only by shutdowns |

Days are stored as whole hundredths of a day (integers, never floating point), so
balances such as 14.6 or -1.44 add up exactly. Adjustments and allowances are typed as
decimals with at most two places (`1.5`), and every page, email and MCP result shows
them as such. Pending days are reserved, so the same days can't be requested twice. Leave costs only
**working days** (Mon–Fri by default, set on **Admin → Settings**) that aren't SA public
holidays. With Tuesday to Friday and Sunday ticked, for example, a Monday is free and a
Sunday costs a day. The holidays are computed from the Public Holidays Act,
including the Sunday-to-Monday rule. Admins add proclaimed days, such as election days,
by hand. Days count against the cycle the leave starts in.

**Balance visibility.** `leave_types.show_balance` controls whether staff see a type's
balance on home or through MCP `my_balances`. Compassionate, unpaid, parental and
maternity are seeded hidden; migration 0005 applies the same defaults to existing
types. Admins can change **Show the balance to staff** under **Admin → Leave types**.
Balance-hidden types that staff may apply for have their names shown in the picker and
listed in MCP `other_types`. Successful applicant previews omit the balance figures
(MCP `available_before` and `available_after`); a refusal still states the figure needed to explain
the limit. Admins keep
all balances, including on person pages, adjustment and recording forms, and MCP tools.

**Admin-only leave types.** `leave_types.staff_can_apply` independently controls
self-service applications. Migration 0007 and fresh seeds turn it off for compassionate
and unpaid leave; admins can change **Staff can apply for it** under **Admin → Leave
types**. These types disappear from the apply picker and MCP applicant suggestions.
The service refuses them even for admins applying for themselves:
“Compassionate leave is arranged with HR, so it cannot be applied for here. Apply for
another type with a note saying why, and HR can change it.” (using the type's name).
Existing requests are untouched: owners editing one keep its type, HR-allowed drafts
keep theirs, and approval, withdrawal and cancellation still work. Admins can record
these types for past leave, and correct a request that is not yet approved into one
(see **Correcting a request (admins)** below).

**0007 rehearsal and rollback (operator).** Before deploying, take a backup and
rehearse on a separate copy, never the live database:
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0007.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0007.db ".backup /data/leave-rehearsal.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; SELECT code, active, show_balance, staff_can_apply FROM leave_types ORDER BY sort; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
```
Expect `6 -> 7`; counts of `users`, `requests`, `request_ranges`,
`balance_adjustments` and `request_steps` must be unchanged. Check
`SELECT code, active, show_balance, staff_can_apply FROM leave_types ORDER BY sort`:
exactly compassionate and unpaid must have `staff_can_apply = 0`.
Before deploy, list the existing live requests of restricted types for the user
(they stay valid):
```sh
kamal app exec --reuse 'sqlite3 -bail -readonly /data/leave.db "SELECT r.id, r.status, t.code FROM requests r JOIN leave_types t ON t.id = r.type_id WHERE t.code IN ('"'"'compassionate'"'"','"'"'unpaid'"'"') AND r.status IN ('"'"'pending'"'"','"'"'awaiting_hr'"'"','"'"'draft'"'"');"'
```
Run `PRAGMA integrity_check` and `PRAGMA foreign_key_check`; repeat `-migrate`
and expect `7 -> 7`. The old container's explicit type projection and UPDATE
leave the new column alone; its INSERT defaults to 1. During the brief swap it
still offers and accepts requests for both types. Re-run the live-requests query
above after deploying to catch requests accepted by the old container during the swap.
Behavioural rollback: tick **Staff can apply for it**. Fix forward by default.
For an image rollback, run from the repo root (discards only these flags, not requests):
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0007.db"'
kamal app exec --reuse "sqlite3 -bail -cmd '.timeout 10000' /data/leave.db \"$(cat internal/db/testdata/undo_0007.sql)\""
kamal rollback
```
Between the undo and `kamal rollback`, the new container fails `leave_types` reads
for those few seconds. Do not restart it in between: it would reapply 0007.

**Correcting a request (admins).** Staff cannot apply for compassionate or unpaid leave,
so they apply as another type with a note, and an admin fixes it. On a request page,
**Correct this request** (or MCP `correct_request`) lets an admin change the **type,
dates (up to 3 stretches), half days and note** of someone else's request. It works
while the request is **pending, waiting for HR, an HR-allowed draft, or approved**
(including leave already started or over); not once it is declined, withdrawn or
cancelled, not for recorded leave or a company
shutdown's days, not while a cancellation of it is pending, and not for a pending
request that someone has already begun to approve through special permission ("Someone
has started approving this"). An admin never corrects their own request: another admin
does. Any type offered to the person is allowed, including the admin-only ones (maternity
is still refused to someone recorded as male); the request's own type is kept even if
it has since been retired.

A **reason is required**. It is kept, with the request as it was and as it is now, in
`request_corrections` (every correction is kept, oldest first) and shown to everyone who
can see the request in a **Corrections** panel. The person is emailed what changed and
why, and so is the approver while it is pending (the admins' queue gets it when the
request sits there). The request keeps its id, status, approver, special-permission flag
and place in the queue; the days are recounted, and its fingerprint changes, so an
approver (or admin) holding the old page is told it changed instead of deciding on it.
A correction never checks coverage limits, the balance (a shortfall sets the
"below zero" flag instead), the backdating limit or the 31-day span, and the dates may be
past or future; overlap with the person's other leave is still refused.

**Correcting approved leave.** The request stays approved, with the same approver and
decision date; the days are recounted and the balance follows (the old type gets its days
back, the new one counts them). Approved leave is on the shared calendar, so the
correction deletes its Outlook events and creates new ones for the corrected type and
dates: the correction transaction reads the event ids off the old date ranges before
replacing them and queues one `calendar_create` job carrying them
(`delete_event_ids`). The job deletes those events first (a Graph 404 counts as done),
then creates one event per range, with a `-c<n>` suffix on the Graph `transactionId` (n =
corrections so far) so Graph cannot hand back an event it just deleted. A retry
re-deletes and re-creates only what is missing. Only the person is emailed (the email says the calendar
is updated); a correction is refused while a cancellation of the leave is pending.
During a deploy swap the old (11c) worker ignores `delete_event_ids` and uses the key
`leave-request-<id>` with no `-c<n>` suffix, so it can record the original, still-live
event on the corrected request's first range and leave Outlook showing the old type and
dates. Avoid correcting approved leave during the swap window. If it happened, make one
more correction of that request: it captures the recorded ids, deletes those events and
creates new ones. Or delete the old events by hand (as in the Outlook clean-up below)
**and** blank that request's `request_ranges.calendar_event_id`, then retry or queue a
`calendar_create`; deleting the events alone would remove the leave from the calendar
while the range still holds the id.

**0008 rehearsal and rollback (operator).** `0008_request_corrections.sql` only adds the
`request_corrections` table. Before deploying, take a backup and rehearse on a separate
copy, never the live database:
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0008.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0008.db ".backup /data/leave-rehearsal.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; SELECT count(*) FROM request_corrections; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
```
Expect `7 -> 8` on the first `-migrate`; the counts of `users`, `requests`,
`request_ranges`, `balance_adjustments` and `request_steps` must be unchanged,
`request_corrections` must be `0`, and both integrity and foreign-key checks clean.
The repeated `-migrate` must report `8 -> 8`. The previous container never reads the
table, so it is harmless during the swap. Fix forward by default. For an image
rollback, run from the repo root (it discards only the correction history, never a
request):
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0008.db"'
kamal app exec --reuse "sqlite3 -bail -cmd '.timeout 10000' /data/leave.db \"$(cat internal/db/testdata/undo_0008.sql)\""
kamal rollback
```
**Warning:** between the undo and `kamal rollback`, the running new container returns
500 on every request page (`no such table: request_corrections`) and the MCP
`get_request` tool fails, for those few seconds. Do not restart it in between: it would
reapply 0008.

**Stand-ins for approvals (core; nothing in the UI or MCP creates one yet).** An approver
who is going away names a stand-in for a date range of at most 30 days (first and last
day both counted); one stand-in at a time per approver, no chains, and anyone with a
working account can be named. The requests stay with the real approver. While a
delegation is active *today* (unrevoked, inside its dates, app timezone) the stand-in
also sees them in their queue and badge, can open them, and may decide them through the
same code paths (pages, bulk approve, MCP). Requests already waiting when it starts
appear at once; whatever is undecided when it ends is the approver's alone again. A
stand-in never decides the leave of whoever set the delegation up, never their own
leave, and never the admins' queue, and gets no administrator powers. An escalated
request is routed as the *delegator's* approval would be, so a stand-in for the final
approver can give the final special-permission approval. Decisions are shown as
"Approved by B for A" (request page, history, the decided email, the calendar event) and
stored in `requests.decided_for_id` / `request_steps.on_behalf_of_id`. While a
delegation is active the stand-in is also emailed what is submitted, edited, escalated,
reassigned or corrected for the approver, with "You are approving for A until ...", and
both are emailed when it is created or revoked by someone else. The service methods are
`Delegate`, `RevokeDelegation`, `Delegations`, `ActingFor`, `CanDecideNow` and
`CanViewNow`; the pages and tools to use them arrive in the next phase.

**0009 rehearsal and rollback (operator).** `0009_approval_delegations.sql` adds the
`approval_delegations` table and two nullable columns (`requests.decided_for_id`,
`request_steps.on_behalf_of_id`). Before deploying, take a backup and rehearse on a
separate copy, never the live database:
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0009.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0009.db ".backup /data/leave-rehearsal.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM request_ranges; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM request_steps; SELECT count(*) FROM approval_delegations; SELECT count(*) FROM requests WHERE decided_for_id IS NOT NULL; SELECT count(*) FROM request_steps WHERE on_behalf_of_id IS NOT NULL; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
```
Expect `8 -> 9` on the first `-migrate`; the counts of `users`,
`requests`, `request_ranges`, `request_steps` and `balance_adjustments` must be
unchanged, `SELECT count(*) FROM approval_delegations` must be `0`,
`SELECT count(*) FROM requests WHERE decided_for_id IS NOT NULL` and
`SELECT count(*) FROM request_steps WHERE on_behalf_of_id IS NOT NULL` must be `0`, both
integrity and foreign-key checks clean, and the repeated `-migrate` must report
`9 -> 9`. During the swap the previous container never reads the table and its INSERTs
name explicit columns, so the new columns stay NULL; a stand-in simply cannot act for
those seconds. Fix forward by default. For an image rollback, run from the repo root
(it discards only the delegations and the "for A" audit, never a request):
```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0009.db"'
kamal app exec --reuse "sqlite3 -bail -cmd '.timeout 10000' /data/leave.db \"$(cat internal/db/testdata/undo_0009.sql)\""
kamal rollback
```
**Warning:** between the undo and `kamal rollback`, the running new container fails every
page that reads a request (`no such column: r.decided_for_id`) for those few seconds. Do
not restart it in between: it would reapply 0009.

**0010 rehearsal and rollback (operator).** `0010_session_last_seen.sql` adds
`sessions.last_seen` and backfills existing sessions to the migration time.
Take a fresh backup, copy it off-host, and rehearse on a separate copy:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0010.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0010.db ".backup /data/leave-rehearsal-0010.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal-0010.db "SELECT count(*) FROM sessions;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal-0010.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal-0010.db "SELECT count(*), min(last_seen), max(last_seen) FROM sessions; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal-0010.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
```

Expect `9 -> 10`, the same number of sessions (all marked seen at migration
time), integrity `ok` and no foreign-key rows, then `10 -> 10`.

The version-9 image refuses a version-10 database, and the binary cannot run
the Down migration. Fix forward by default. To roll back, run from the repo
root: take a safety backup, then drop `sessions.last_seen` and remove the
version-10 goose row together using the undo script.

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0010.db"'
kamal app exec --reuse "sqlite3 -bail -cmd '.timeout 10000' /data/leave.db \"$(cat internal/db/testdata/undo_0010.sql)\""
kamal rollback <version>
```

**Warning:** session reads and writes can fail between the undo and the image
swap; do not restart the new image in between, or it will reapply 0010.
Alternatively, restore the version-9 pre-deploy backup using the restore
procedure below (including Outlook event cleanup), then use
`kamal rollback <version>` instead of `kamal app boot`. A restore loses writes
since the backup. Either rollback signs everyone out again because the old image
uses a different session cookie name.

**0011 outbox lease rehearsal, deploy and rollback (operator).** The migration
adds two nullable columns to `outbox`; existing jobs, attempts and schedules
are unchanged. Run from the repo root. Set `VERSION` to the new image tag and
`OLD` to the currently running schema-10 tag (`kamal app version`). Once the
new image is available on the Docker host, back up the running app, copy the
backup off-host (see the backup procedure below), and rehearse on a separate copy:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0011.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0011.db ".backup /data/leave-rehearsal-0011.db"'
CHECK='SELECT count(*), coalesce(sum(done_at IS NULL AND failed_at IS NULL),0), coalesce(sum(failed_at IS NOT NULL),0) FROM outbox; PRAGMA integrity_check; PRAGMA foreign_key_check;'
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave-rehearsal-0011.db '$CHECK'"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$VERSION -migrate-status"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$VERSION -migrate"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$VERSION -migrate"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$VERSION -migrate-status"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave-rehearsal-0011.db '$CHECK SELECT count(*) FROM outbox WHERE lease_until IS NOT NULL OR claim_token IS NOT NULL;'"
ssh sdt-vm-docker02 "docker run --rm -i -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave-rehearsal-0011.db" < internal/db/testdata/undo_0011.sql
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$OLD -migrate-status"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 -e DB_PATH=/data/leave-rehearsal-0011.db ghcr.io/17xande-dev/leave:$OLD -migrate"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave-rehearsal-0011.db '$CHECK'"
```

Expect `0011 pending`, `10 -> 11`, `11 -> 11`, `0011 applied`, then only
versions through 0010 and `10 -> 10` with the old image after undo. The total,
pending and dead counts must stay unchanged; after Up the lease count must be
zero. Every integrity check must return `ok` and every foreign-key check must
return no rows. Never start the server against the rehearsal copy: it would
run copies of live jobs.

**Stop first:** run `kamal app stop` and verify the old container is stopped
before `kamal deploy`. An already running old worker ignores leases even
though its SQL still works against the additive schema; it could send the same
job as the new worker. A fresh start of the old binary refuses schema 11.
New workers can overlap with each other, but delivery is still at least once:
a crash after a remote service accepts an effect and before it is recorded can
repeat it.

To roll back, keep the app stopped for the backup, undo and checks. Run these
steps in order, stopping if any command fails or a check gives the wrong result:

```sh
kamal app stop
kamal app details  # verify no leave-web container is running before continuing
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave.db '.backup /data/leave-before-undo-0011.db'"
ssh sdt-vm-docker02 "docker run --rm -i -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave.db" < internal/db/testdata/undo_0011.sql
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail /data/leave.db 'PRAGMA integrity_check; PRAGMA foreign_key_check;'"
ssh sdt-vm-docker02 "docker run --rm -v leave-data:/data --user 10001 ghcr.io/17xande-dev/leave:$OLD -migrate-status"
kamal rollback "$OLD"
```

Before `kamal rollback`, require `ok` and no foreign-key rows, and require the
old image to report 0010 as the newest applied migration. The undo script
drops the two lease columns and deletes only the `version_id = 11` row from
`goose_db_version`, in one transaction. It leaves earlier rows intact and does
not rewind the table's `sqlite_sequence` counter. The old image sees version
10 and can start. Job attempts and delivery history are unchanged; unfinished
jobs lose their leases and return to the old scheduling rules.

**Never use `kamal app exec` without `--reuse` for this rollback.** It keeps
the image's `leave` entrypoint and would start the real server and its worker,
not `sqlite3`. The explicit `--entrypoint sqlite3` runs only the SQLite client;
UID 10001 is the database owner. Do not restart the new server after undo:
it would reapply 0011. Restoring the pre-deploy backup instead loses later
writes and requires the Outlook-event cleanup in the restore procedure below.

**Several stretches in one request.** Apply, edit and admin Record leave accept up to
3 separate date ranges for working-day leave. Touching or overlapping ranges merge.
Applicants' working-day leave must fit within 31 days from its first to last day,
including a single stretch; admin recording and imports are exempt from this guard.
Calendar-day types and weekends off remain one continuous stretch. The restricted-day
minimum applies to each stretch separately. Half-day flags mean the first day of the
first range and the last day of the last range. Gap days cost nothing and stay free in
the team view, coverage checks and picker marks. Each approved range has its own shared
calendar event. Without JavaScript the form still accepts one first/last-day pair.

The picker supports click-click selection, drag selection and removing a day to split
a stretch; Done closes it. Its upstream is `~/dev/libraries/date-range-input`: change
the widget there and re-vendor it into `internal/handler/static/`, recording its commit,
size and SHA-256 in that directory's README.

MCP `apply_for_leave`, `preview_request`, `edit_request` and `record_leave` accept
`ranges: [{start, end}]` instead of `start`/`end`; sending both is an error. Request
results include `ranges`, while `start` and `end` remain the span bounds. The admin CSV
also includes a `ranges` column alongside its span columns.

**Half days** start OFF, including after upgrading an existing database. Under
**Admin → Settings**, tick **Let people book half days** to offer them on apply/edit
forms and through assistants. Admins can always record half days and change them on
another person's live working-day leave from its request page, including approved leave.
The change is audited and emailed to the person, and to the approver when pending.
Existing requests
keep their half days and can still be approved; accepted drafts can still be submitted.
With the setting off, editing a pending request offers whole days only, so saving it
removes its half-day flags. Turning it on restores half-day booking.

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

### Payroll export

On **Admin → People**, **Download payroll export (Excel)** downloads an admin-only
`.xlsx` report as at **today** in the configured timezone. Enabled accounts are
included by default; tick **Include disabled accounts** for leavers too.
**Annual leave** contains each person's allocation, adjustments, approved and
pending leave, and the October 2026 payroll opening figure where recorded.
**All types** includes balances hidden from staff and inactive types with current
activity; **About** explains the columns and each type's current cycle.

**Available** is what the person can still book: entitlement less approved and
pending leave, including future bookings. **Balance to date** ignores pending
and future approved leave; a request that has started counts whole. Payroll can
choose the column matching its payslip convention. Opening balances are blank
where the import needed no correction; change since opening is Available less
that opening figure. The app counts each person's work week and long weekend
less South African public holidays, so its day counts can differ from payroll's.

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
| `approval_queue` | What is waiting for their decision, with coverage warnings, who it goes to next, the requester's balance afterwards (`balance_after`, absent for unlimited types) and the trail; `for` names the approver when the person may decide the row only as their stand-in |
| `decide_request` | Approve or decline (a decline needs a note). `expected` is optional: pass the `fingerprint` from `get_request` or `approval_queue` and the decision is refused if the person has edited the request since |
| `my_delegations` | Their stand-ins (who approves for them while they are away), who they are standing in for, and their recent past ones, with the ids `revoke_delegation` takes. Administrators can pass `approver` to look at someone else |
| `delegate_approvals` | Name a stand-in for their approvals: `delegate`, `start`, `end` (at most 30 days), optional `note`. Open to everyone for their own; `approver` (someone else) is for administrators only. The stand-in is emailed. Only when the person has asked for it |
| `revoke_delegation` | End a stand-in now (the approver or an administrator): destructive, the stand-in is emailed |
| `all_requests` | Administrators only: every request, filtered by status, dates and person |
| `who_is_away` | Administrators only: who is away in a date range, by department |
| `outbox_problems` | Administrators only: failing emails and calendar jobs |
| `person_balances` | Administrators only: a person's balances for the current cycle and every adjustment made for them |
| `permission_queue` | Administrators only: requests waiting for HR to allow a negative balance (`/admin/permissions`), with the shortfall |
| `record_leave` | Administrators only: record leave a person has already taken (the **Record leave** form): its last day must be today or earlier. A note is required; a retired type is allowed; the balance may go below zero and the result says so. The person is emailed |
| `remove_recorded_leave` | Administrators only: remove leave an administrator recorded; it no longer counts against their balance and the person is emailed |
| `adjust_balance` | Administrators only: add or remove days (fractions allowed, like 1.5) on one type's balance for a cycle (default the current one), with a required reason. Nobody is emailed |
| `grant_negative` | Administrators only (destructive: it cannot be recalled): allow a request awaiting HR to go below zero; it returns to the person as a draft and they are emailed. The note is optional |
| `refuse_negative` | Administrators only: decline a request awaiting HR; a note is required and the person is emailed |
| `reassign_request` | Administrators only: move a pending request (including your own) to an enabled administrator other than the applicant, or with none to the leave administrators; the new approver is emailed |
| `correct_request` | Administrators only: change the type, dates, half days and note of someone else's request that is pending, waiting for HR, an HR-allowed draft or approved (approved: the shared calendar events are recreated), a reason required; works on admin-only types, refused for your own request; the person (and the approver while pending) is emailed. `get_request` shows `can_correct` and the `corrections` history |

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
deployment is safe: the container listens on its own network address on the `kamal`
docker network (nothing is published on the host) and gets the public `Host`. It breaks if the binary
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
make recreate-db         # discard a disposable local database
make migrate-status      # inspect migrations without starting the server
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
  CA's PEM into `/home/it_admin/leave/certs/` on the server (mounted read-only at
  `/certs` by `config/deploy.yml`) and set `LDAP_CA_FILE=/certs/<file>.pem`. Use the DC's full
  name in `LDAP_URL`, because that is what its certificate names. Check the setup with
  the `LDAP_LIVE=1` test rather than `ldapsearch` or `openssl`: recent OpenSSL refuses
  a 1024-bit DC key that the app itself accepts.
- Keep AD's account-lockout threshold above what the app's own sign-in limit lets
  through, so a guesser hits the app's limit before a colleague is locked out of
  Windows. The limit is a token bucket per username: a burst of 5 attempts, then one
  more every 3 minutes. That is at most 6 attempts in 3 minutes and 10 in 15 minutes,
  and about 20 more per hour after that. Set AD's threshold above the count for the
  lockout observation window you use (a 15-minute window needs a threshold of 11 or
  more). A separate limit per client address (an IPv6 client is counted per /64; burst 20, one
  every 10 seconds) slows spraying many usernames from one place.

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

Create a client secret and put it in your local `.env` (then redeploy). It expires, so set a calendar reminder to
renew it. When it lapses, every email and calendar update piles up on the Outbox page
until the new secret is in place, then Retry clears them.

### 3. Deploying (Kamal)

The app runs on `sdt-vm-docker02` behind a shared `kamal-proxy`, reached through one shared
Cloudflare Tunnel. Host-side setup (kamal-proxy and the tunnel's `cloudflared`) lives in
`~/dev/riverschurch/infra/sdt-vm-docker02`; this repo only deploys the app. `make deploy` runs the check
gate, then `kamal deploy`: build the image locally (`linux/amd64`), push it to
`ghcr.io/17xande-dev/leave`, and swap it in behind kamal-proxy once `GET /healthz`
answers. It refuses to run with uncommitted changes, because Kamal builds from `HEAD`.

**Deploys requiring a stop:** `kamal app stop` first, then `kamal deploy` for
the move to WAL (see "Known limitation" below) and for the first deploy with
outbox leases (see the 0011 rehearsal and rollback instructions above).

One-time setup, on your workstation:

1. **`sdt-vm-docker02` in `~/.ssh/config`**, with user `it_admin` (config/deploy.yml sets the
   user too). That user needs passwordless `docker`.
2. **`gh` logged in** with a token that can write packages: `.kamal/secrets` uses
   `gh auth token` as the ghcr.io password.
3. **A local `.env`** (git-ignored) copied from `.env.example` and filled in. `.kamal/secrets`
   reads the LDAP and Graph values from it with `grep`, so run `make` / `kamal` from the repo
   root. The non-secret settings (`BASE_URL`, `COOKIE_SECURE`, `TRUSTED_PROXIES`, `TZ_NAME`)
   are in `config/deploy.yml`. Session lifetime (2 h idle, 12 h absolute) is not configurable; a
   leftover `SESSION_TTL` from an older `.env` is ignored. Values must be unquoted, with no inline `# comment`, no
   trailing spaces, no duplicate keys and no `$`: Kamal's secrets parser would otherwise
   rewrite them silently. `bin/check-env` checks this (and that required keys are set); it
   runs from `make deploy` and from the `.kamal/hooks/pre-connect` hook, so a raw
   `kamal deploy` is covered too. An optional setting (`LDAP_TEAMS_BASE_DN`, ...) reaches the
   server only if you add it to `env.secret` and `.kamal/secrets`.
4. **The LDAP CA on the server**: put the PEM in `/home/it_admin/leave/certs/` (bind-mounted
   read-only at `/certs`, and untouched by deploys) and set `LDAP_CA_FILE=/certs/<file>.pem`.

Then `make deploy`. Logs: `make logs-prod`. Other commands (`kamal app details`,
`kamal rollback <version>`, `kamal app exec --reuse -i sh`) work as usual; `kamal config` shows
what would be deployed.
After later migrations ship, rolling back to a goose-enabled image older than the
last-applied migration deliberately refuses to boot with the newer-schema guard ("written by
a newer build"; images before the sqlitekit move said "newer than this binary"); this is expected protection against running code on an incompatible schema.

Numbered SQL migrations in `internal/db/migrations` are embedded and applied at startup
by goose. Add each schema change as a new migration; never edit an applied migration.
`go run . -migrate-status` reports applied/pending migrations without applying them or
starting the server (opening the file still switches it to WAL, which is harmless). `go run . -migrate` applies pending migrations, prints status and
exits, without loading LDAP/Graph configuration or starting the server. Both use
`DB_PATH` (default `data/leave.db`). `-migrate` requires an existing database file;
a missing path exits with code 1 without creating it. An existing empty database
reports all eleven migrations pending until startup or `-migrate`. A database newer than
the binary is refused by both.

**Rehearse 0005 balance visibility from schema 0004.** These are operator instructions;
historical, for an image whose latest migration is 0005 (not the current image).
run them only on the host rehearsal copy after taking a fresh pre-deploy `.backup`
and copying it off-host using the backup procedure below. With a checked, pushed
image selected by `$VERSION`, record the following counts on the copy before migration:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-rehearsal.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) AS users FROM users; SELECT count(*) AS requests FROM requests; SELECT count(*) AS request_ranges FROM request_ranges; SELECT count(*) AS balance_adjustments FROM balance_adjustments; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) AS users FROM users; SELECT count(*) AS requests FROM requests; SELECT count(*) AS request_ranges FROM request_ranges; SELECT count(*) AS balance_adjustments FROM balance_adjustments; SELECT code, active, show_balance FROM leave_types ORDER BY sort; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
```

Expect `4 -> 5`, all four counts unchanged, exactly `compassionate`, `unpaid`,
`parental` and `maternity` at `show_balance=0`, and every other type at `1`.
Require integrity `ok` and no foreign-key rows. Repeat the same `-migrate` command:
expect `5 -> 5` and no changed counts or flags. The column does not exist before
migration, so inspect it only afterwards. Verify a staff home has no rows for the
four types, the apply picker still offers them by bare name, and the admin person
page keeps their balances.

If Phases 5/7 have not yet been deployed, this image carries them too. In the same
rehearsal run Phase 5's pending read-only check for live workday-unit requests or
drafts spanning at least 31 days, and review any matches before deployment:

```sql
SELECT r.id, r.user_id, r.status, r.start_date, r.end_date
FROM requests r JOIN leave_types t ON t.id = r.type_id
WHERE r.kind = 'leave' AND r.status IN ('draft', 'pending', 'awaiting_hr', 'approved')
  AND t.unit = 'workdays'
  AND julianday(r.end_date) - julianday(r.start_date) + 1 >= 31;
```

**Rollback after 0005.** For a behavioural rollback, tick **Show the balance to
staff** on the affected types; no deployment is needed. An image supporting only
0004 refuses schema 5, so an image rollback requires fixing forward or restoring
the pre-deploy backup. A restore loses writes since that backup and requires the
Outlook event cleanup described below. The tested Down migration is not the
production rollback procedure.

**Rehearsal when also applying 0003/0004.** These are operator instructions, not an
automatic deployment step. This historical routine targets an image ending at 0005;
for the current image also follow the 0006 precheck and comparison below.
Start with a committed, checked image; build and push it
without swapping the running app (`VERSION=$(git rev-parse HEAD)` then
`kamal build push --version "$VERSION"`). Take a fresh `.backup` of the live database
using the running container and copy it off the host using the backup procedure below.
Make a separate rehearsal copy on the host's volume:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-rehearsal.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM balance_adjustments; SELECT count(*) FROM requests WHERE status IN ('"'"'pending'"'"','"'"'draft'"'"','"'"'awaiting_hr'"'"') AND (half_start=1 OR half_end=1); PRAGMA integrity_check; PRAGMA foreign_key_check;"'
ssh sdt-vm-docker02 "docker pull ghcr.io/17xande-dev/leave:$VERSION"
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
kamal app exec --reuse 'sqlite3 -bail /data/leave-rehearsal.db "SELECT count(*) FROM users; SELECT count(*) FROM requests; SELECT count(*) FROM balance_adjustments; SELECT half_days_enabled FROM settings; SELECT count(*) FROM requests WHERE halves_set_by_id IS NOT NULL; PRAGMA integrity_check; PRAGMA foreign_key_check;"'
```

Expect `migrating /data/leave-rehearsal.db: 2 -> 5` before the applied status lines
(or `3 -> 5` when 0003 is already applied);
verify that the resolved path names the rehearsal copy and the versions match the
intended upgrade. A repeat run reports `5 -> 5`.

Run the pushed image directly on the host: Kamal's parser rejects the documented
`app exec --version ... --env ... -- '-migrate'` form before it can run. Pulling
first makes the selected image available (Docker can also pull it on demand).
The disposable container mounts the same named volume but points `DB_PATH` at
the rehearsal copy. Pass **only `-migrate`** to the image's `leave` entrypoint.
The `--reuse` commands above inspect the copy through the running container;
they do not run the new binary.

Require `0001` through `0005` applied, unchanged counts, `half_days_enabled=0`,
`SELECT count(*) FROM requests WHERE halves_set_by_id IS NOT NULL` returning `0`, integrity
`ok`, and no foreign-key rows. If any pending/draft/awaiting-HR request has half flags,
tell the user before deploying. Stop on any discrepancy. Only after the rehearsal is
accepted, deploy normally, then check `leave -migrate-status`, `/healthz`, the same
counts and integrity checks on the live database, and the setting/form behavior.
Remove the in-volume backup and rehearsal copies after verification; retain the
off-host backup. Never point the rehearsal's `DB_PATH` at `/data/leave.db`.

**0004 expanded storage; 0006 contracts it.** The contract step, called 0005
in 0004's comments, is 0006; applied migrations are unchanged. 0004 copied each
request's dates and calendar id into one range and supplied transitional insert/edit
triggers and a cascading foreign key for pre-range binaries. 0006 adopts remaining
legacy ids into empty first ranges, blanks matching ids, guards the data, and removes
both triggers, the cascade, the legacy column and its application shim.
`requests.start_date`/`end_date` stay as the indexed span for ordering and policy,
written with the ranges by `insertRanges`. `days_x100` stays authoritative: HR's
recorded charge and quote-time work weeks, holidays and half flags cannot be
recomputed from range dates in SQL.

**Rehearse 0006 (contract) from schema 0005.** Operator instructions only; never
point this rehearsal's `DB_PATH` at `/data/leave.db`. Build and push the checked
image, take a fresh off-host backup using the procedure below, then make two
identical in-volume copies (the comparison base must remain untouched):

```sh
VERSION=$(git rev-parse HEAD)
kamal build push --version "$VERSION"
# Take and copy the fresh off-host backup before continuing.
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-pre-0006.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave-pre-0006.db ".backup /data/leave-rehearsal.db"'
ssh sdt-vm-docker02 "docker run --rm -i -v leave-data:/data --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail -readonly /data/leave-rehearsal.db" < internal/db/testdata/precheck_0006.sql
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
ssh sdt-vm-docker02 "docker run --rm -i -v leave-data:/data --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail -readonly /data/leave-rehearsal.db" < internal/db/testdata/verify_0006.sql
ssh sdt-vm-docker02 "docker run --rm -i -v leave-data:/data --entrypoint sqlite3 ghcr.io/17xande-dev/leave:$VERSION -bail -readonly /data/leave-rehearsal.db" < internal/db/testdata/compare_0006.sql
ssh sdt-vm-docker02 "docker run --rm --env DB_PATH=/data/leave-rehearsal.db -v leave-data:/data ghcr.io/17xande-dev/leave:$VERSION -migrate"
```

The stdin-over-ssh form above is **untested on the host**; the SQL files are tested
locally. Fallback: paste their statements into
`kamal app exec --reuse 'sqlite3 -bail -readonly /data/leave-rehearsal.db "..."'`.
`kamal app exec --version ... --env` does not work; use the ssh migration form.
Record the precheck counts (recent production: 179 users, 878 requests/ranges,
173 adjustments). All five guard counts and `distinct` must be zero. If
`distinct > 0`, tell the user: it is a duplicate Outlook event from the 0004 swap.
Resolve by deleting that distinct event in Outlook and blanking
`requests.calendar_event_id` for that request on live, then repeat the copies and
rehearsal. Do not blindly blank a distinct id.

Expect `5 -> 6`, no legacy column or transitional triggers, FK `NO ACTION`,
the date index, zero invariant violations, integrity `ok` and no foreign-key rows.
Requests and ranges counts must match precheck; non-empty range ids include
adoptions. Every compare result must be zero. Repeat migration expects `6 -> 6`.
Stop on any non-zero exit or discrepancy and tell the user before deploying.

**During the deploy swap.** Confirm the running image is range-aware (0005).
The old container can run for seconds after migration, worst case about 25 seconds
with `stop_timeout: 20`. Its `SELECT calendar_event_id FROM requests WHERE id=?`
fails before any Graph call: calendar jobs retry after a minute, with no duplicate
or orphan event. Its legacy-clearing `UPDATE requests SET calendar_event_id='' ...`
can fail after a Graph deletion; the transaction retains the range id, and retry
treats Graph's 404 as success. All date writers and shutdown deletion already own
their ranges.

**If the deploy fails after 0006 applied.** The new container migrates at boot,
before its health check. If it never becomes healthy, Kamal keeps the old 0005
container serving a v6 database, and the 25-second figure above no longer holds:
every Outlook create/delete fails before any Graph call and retries for about a
day (12 attempts) before landing in "failed". The old container cannot restart
(its boot guard refuses v6). No data is lost; events are only late. Check with
`leave -migrate-status`, then fix forward quickly or run the undo below, and
retry any failed calendar jobs from the outbox admin page.

**Recovery after 0006.** Fix forward by default. For an image rollback, first
preserve a fresh `.backup`, then apply the tested lossless Down script in one
transaction through the running container:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0006.db"'
kamal app exec --reuse "sqlite3 -bail -cmd '.timeout 10000' /data/leave.db \"$(cat internal/db/testdata/undo_0006.sql)\""
kamal app exec --reuse 'sqlite3 -bail -readonly /data/leave.db "PRAGMA integrity_check; PRAGMA foreign_key_check;"'
kamal rollback <version>
```

If the undo reports "database is locked" it changed nothing: retry it. Rehearse
the undo on `/data/leave-rehearsal.db` first (undo, then `-migrate` again
expecting `5 -> 6`). Require integrity `ok` and no foreign-key rows before rollback. Down preserves
multi-range requests and event ids, restores both triggers and CASCADE, and appends
an empty legacy column (so the old shim has nothing to adopt). The running 0006
binary is safe meanwhile: it still writes its ranges and tolerates the restored
triggers. Do not restart it between undo and rollback or it reapplies 0006.
Restoring the pre-deploy backup is the last resort: it loses writes and needs the
Outlook cleanup documented for 0004 below.

After deploy, check `kamal app exec --reuse 'leave -migrate-status'` (0006 applied),
`/healthz`, and run `verify_0006.sql` read-only on live by replacing only the sqlite
path in the verify command above with `/data/leave.db`. Check the outbox admin page
for failed calendar jobs after the one-minute swap retry. Remove both in-volume
copies (`/data/leave-pre-0006.db`, `/data/leave-rehearsal.db`) after verification;
retain the off-host backup.

**Rollback after 0004 means restoring the pre-deploy backup**, losing writes
since that backup, or fixing forward. A previous image refuses schema version
4; `kamal rollback` alone cannot recover it. Stop writes, preserve a backup of
the failed state, and use the restore procedure below before starting the
previous image. The guarded Down exists for testing and refuses multiple
ranges; it is not the production rollback procedure.
Before restoring, export the range event ids and manually delete the Outlook
events for requests created or approved since the backup, as described below.

**Recovery if deployment fails after migration 0003.** Stop writes and back up the
migrated database first. To return to an image supporting 0002, remove 0003's audit
columns and version record in one transaction using an existing container with
access to the volume:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0003.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave.db "BEGIN IMMEDIATE; ALTER TABLE requests DROP COLUMN halves_set_by_id; ALTER TABLE requests DROP COLUMN halves_set_at; DELETE FROM goose_db_version WHERE version_id=3; COMMIT;"'
```

This discards the half-day correction audit, retaining the flags and charges.
Check integrity and foreign keys as above, then roll back to the 0002 image.
Do not restart the 0003 image during recovery: it would reapply the migration.
For an image older than 0002, undo 0003 first, then follow the 0002 recovery below.
Alternatively, restore the pre-deploy backup; that loses changes since the backup.

**Recovery if deployment fails after migration 0002.** The previous binary refuses
to start with `database migration version 2 is newer than this binary`. Stop writes
and back up the migrated database before recovery. If 0002 is the only migration
to undo, manually remove its column and version record in one transaction using
an existing container with access to the volume:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db ".backup /data/leave-before-undo-0002.db"'
kamal app exec --reuse 'sqlite3 -bail /data/leave.db "BEGIN IMMEDIATE; ALTER TABLE settings DROP COLUMN half_days_enabled; DELETE FROM goose_db_version WHERE version_id=2; COMMIT;"'
```

This discards the half-days setting; requests and their half-day flags remain.
Check integrity and foreign keys using the queries above, then roll back to the
previous image. Alternatively, restore the pre-deploy backup using the restore
procedure below and start the previous image; that loses changes made since the
backup. Do not restart the new image during recovery: it would apply 0002 again.

An existing database without `goose_db_version` is refused before any migration runs.
For disposable local data, use `make recreate-db`.

**One-time production adoption of baseline 1.** Run these commands from the repo root
on your workstation. The app does **not** need to be stopped: the adoption SQL is safe
while the old app is still serving. Use `--reuse` for every one-off command so Kamal
executes it inside the running container; without it, the image's `leave` entrypoint
receives the command as arguments and can start another server instead.

1. Immediately before adopting, take a fresh SQLite `.backup` and inspect its schema:

```sh
kamal app exec --reuse 'sqlite3 /data/leave.db ".backup /data/leave-before-goose.db"'
kamal app exec --reuse 'sqlite3 /data/leave-before-goose.db ".schema"'
```

Spot-check that backup's tables, columns, constraints and indexes against
[`0001_baseline.sql`](internal/db/migrations/0001_baseline.sql), including `users.sex`,
and resolve any drift before proceeding. Production's `users.sex` was appended by
`ALTER TABLE`, so its physical column order differs from a fresh database; that order
difference is expected. This is a manual check: adoption does not automatically verify
schema equivalence. Repeat the backup and check if any manual schema change happens
before adoption.

2. Apply the exact SQL in
[`internal/db/testdata/adopt_baseline.sql`](internal/db/testdata/adopt_baseline.sql),
which is also exercised by the adoption regression test. The workstation reads the
file and passes its contents as SQLite's SQL argument inside the running container:

```sh
kamal app exec --reuse "sqlite3 -bail /data/leave.db \"$(cat internal/db/testdata/adopt_baseline.sql)\""
```

3. Verify the version rows with SQLite while the old image is still running:

```sh
kamal app exec --reuse 'sqlite3 -bail /data/leave.db "SELECT version_id,is_applied FROM goose_db_version ORDER BY id;"'
```

The output must contain exactly `0|1` followed by `1|1`. Stop on any error or unexpected
result. The old image has no `-migrate-status` flag; do not run it until after deployment.

4. Only after that verification, deploy the committed new image and check its status:

```sh
make deploy
kamal app exec --reuse 'leave -migrate-status'
```

The original baseline-only image reports `0001 applied 0001_baseline.sql`. The current
image also applies and reports `0002 applied 0002_half_days_enabled.sql` and
`0003 applied 0003_halves_audit.sql`; rehearse them
on a copy first as described above. Adoption marks the existing baseline as applied
without executing its DDL. The SQL is a
one-time operation: it deliberately fails if the tracking table already exists. Do
not use `goose up` to adopt an existing schema; `goose fix` only renumbers files and
does not mark migrations applied.

The database lives in the `leave-data` named volume. Back it up and copy it out:

```sh
kamal app exec --reuse 'sqlite3 /data/leave.db ".backup /data/backup.db"'
ssh sdt-vm-docker02 'docker cp "$(docker ps -q -f name=leave-web | head -n1):/data/backup.db" -' > leave-backup-$(date +%F).tar
```

`.backup` uses SQLite's backup API, because a plain `cp` of a live database can capture a
torn state. The database runs in WAL mode, so **while the app runs it is three files**
(`leave.db`, `leave.db-wal`, `leave.db-shm`): never `cp`, `docker cp` or tar the volume of a
running app; use `.backup` (or `VACUUM INTO`) as above, or stop the app first. A fourth file,
`leave.db.migrate-lock`, sits beside them: startup, `-migrate` and `-migrate-status` take an
exclusive lock on it so two processes opening the database together cannot both migrate it.
It is empty; do not delete it while the app runs. Run migrating commands as the image user (e.g.
through `kamal app exec`), never as root: every open takes `leave.db.migrate-lock` read-write, so a
root-created (0600) lock file stops the app starting on its next restart, and makes the status
command and the cmd tools fail with permission denied.

**Automatic daily backups.** At startup and then daily the app writes a `VACUUM INTO` copy of the
database to `/data/backups/leave-YYYY-MM-DD.db` (the date is the UTC day), one per day,
mode 0600 in a 0700 folder, and keeps the newest 14; nothing else in the folder is touched.
`BACKUP_DIR` (default `backups` beside the database file) and `BACKUP_KEEP` (default 14;
`0` turns backups off) change this. A copy is a complete, ordinary database file, so
restoring is `kamal app stop`, replacing `leave.db` with the copy (and removing any
`leave.db-wal` and `leave.db-shm`), then `kamal app boot`. These copies live in the same
volume as the database: they cover mistakes and corruption, **not the loss of the disk**,
so the off-host copy in the procedure above is still needed (an automatic off-site copy is
an open decision below). Take the pre-deploy `.backup` as before; it is not replaced.

To restore or roll the data back, `kamal app stop` first, before copying into or removing
the `leave-data` volume. A leftover `-journal`/`-wal` file next to a copied database can
corrupt it, so copy the database in with no stale sidecar files, then `kamal app boot`.

For a restore after 0004, while writes and workers are stopped and **before
restoring the backup**, export the failed state's range event ids:

```sql
SELECT request_id, calendar_event_id FROM request_ranges WHERE calendar_event_id <> '';
```

Use the export to identify requests created or approved after the backup and
delete their Outlook events by hand **before restoring**. Include requests that
existed but were not yet approved in the backup. Restoring the database does not
remove Outlook events: skipping this cleanup leaves orphan events, and approving
the requests again creates duplicates. Retain the export with the failed-state
backup.

**Cutover / data migration from the old compose stack.** The old compose volume is
`leave_leave-data`; the new one is `leave-data`. Keep the old volume until the new app is
signed off. Do the copy **before** moving the hostname to the shared tunnel, so nothing
written to the empty new database is overwritten:

```sh
# on sdt-vm-docker02, after the first `make deploy` has created `leave-data`
docker compose -f ~/leave/compose.yaml stop leave        # nothing writes to the source
docker run --rm -v leave_leave-data:/from:ro -v "$PWD":/b alpine tar czf /b/leave-data-premigrate.tgz -C /from .
# on the workstation: stop the new app so nothing writes to the target
kamal app stop
# on sdt-vm-docker02: copy EVERYTHING (leave.db and any -journal), then own it as the app's uid
docker run --rm -v leave_leave-data:/from:ro -v leave-data:/to alpine \
  sh -c 'cp -a /from/. /to/ && chown -R 10001 /to'
# on the workstation
kamal app boot
```

`cp -a /from/.` copies a leftover `leave.db-journal` too, which SQLite needs if the old app
was killed mid-write. (The old stack's app is stopped first, so its volume is safe to copy;
the same copy of a running WAL database would not be.) The LDAP CA bundle lives in `/home/it_admin/leave/certs`; never
delete that directory (it is also the old compose `./certs`).

**Known limitation.** During each deploy the old and new containers briefly overlap. The
outbox worker in both may rarely run the same job twice (a duplicate email, for
example). It is low risk and accepted.

**The first deploy after the move to WAL must stop the old container first**: run
`kamal app stop`, then `kamal deploy`. Switching the database file to WAL needs an exclusive
lock, and while the old container is still writing, the new one can exit at startup with
`sqlite settings: sqlitekit: check: database is locked (5) (SQLITE_BUSY)`. It fails safe (the old
container keeps serving and the file is untouched) but the deploy fails. Once the file is in
WAL mode, later deploys need no stop and overlap as described above.

### 4. Public access: the shared Cloudflare Tunnel

The app must be served over HTTPS: its session cookies are `Secure`, and `BASE_URL`
(the public address) is used for CSRF checks, email links and the AI connector's OAuth
issuer. Cloudflare terminates TLS; the tunnel (run by `~/dev/riverschurch/infra/sdt-vm-docker02`) makes an
outbound-only connection to Cloudflare, so no inbound port is opened, and forwards each
hostname to `http://kamal-proxy:80`, which routes by `Host` to the app. kamal-proxy
serves plain http (`proxy.ssl: false`).

What has to exist (once): a public hostname `leave.rivers.tools` on the shared tunnel,
service `http://kamal-proxy:80`, in Cloudflare Zero Trust, with its DNS record.

`TRUSTED_PROXIES=172.30.17.0/24` (set in `config/deploy.yml`) is the `kamal` docker
network's fixed subnet on the host. The peer the app sees is kamal-proxy, which (with
`forward_headers: true` in `config/deploy.yml`) appends its peer to the `X-Forwarded-For`
Cloudflare sent; the app reads only that header, from the right, and only when the peer is in
that subnet, which is how the rate limits see real client IPs instead of the proxy's.
`CF-Connecting-IP` is not read. An invalid CIDR stops startup. This is safe only while the tunnel is the sole way in: nothing may publish a
port on the host that reaches the app or kamal-proxy. If the subnet changes, change this
to match.

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
| Dependencies | `go-ldap/ldap` (LDAP), `modernc.org/sqlite` (pure Go: `CGO_ENABLED=0`, no C toolchain in the image; replaced `mattn/go-sqlite3`), `17xande-dev/sqlitekit` (connection settings and startup check, the same for every SQLite app), `pressly/goose/v3` (numbered SQL migrations; v3.29.0 is the newest release at least two weeks old as of 2026-11-12, with a STRICT-safe version store), `17xande-dev/websec` (CSRF with a body cap, session cookies and lifetime; brings `justinas/nosurf`), `17xande-dev/ratelimit` (client IP and rate limits; uses `x/time/rate`), `17xande-dev/mailer` (Graph mail and tokens), `modelcontextprotocol/go-sdk` (the MCP server: the protocol is still moving and tracking it by hand is a thin reimplementation for one person to maintain). The SDK brings `google/jsonschema-go`, `segmentio/encoding` (with `segmentio/asm`), `yosida95/uritemplate/v3` and `golang.org/x/oauth2`, and raises `x/sync` and `x/sys`. `xuri/excelize/v2` v2.11.1 writes payroll `.xlsx` workbooks: chosen by the user over hand-written OOXML, published 2026-11-02 and at least two weeks old on 2026-11-17. We only write workbooks, never open uploaded ones (where its CVE history lies). It brings `richardlehane/mscfb`, `richardlehane/msoleps`, `tiendc/go-deepcopy`, `xuri/efp`, `xuri/nfp` and `golang.org/x/net` (raised to v0.62.0); its graph also includes `x/image` and the already-present `x/crypto` and `x/text`. The OAuth server is about 600 lines on the standard library, and the Graph calendar client is about 150 lines here rather than the Graph SDK. |

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
| Off-site database backups | Daily `VACUUM INTO` copies in the data volume (14 kept); copied off the host by hand | Losing the disk is the case they do not cover; decide an off-host destination |
| Half days | Not supported (by decision) | Revisit on request |
| Sign-in timing | An unknown username answers slightly faster than a wrong password (no DC bind), which reveals whether a username exists | Usernames are not secret inside the organisation; revisit if that changes |
