CREATE TABLE IF NOT EXISTS users (
	id          INTEGER PRIMARY KEY,
	-- The directory's permanent id for the person (AD objectGUID). Not the
	-- DN: moving someone to another OU changes their DN, and keying on it
	-- would make their next sign-in collide with their own old row.
	ext_id      TEXT NOT NULL UNIQUE,
	-- AD compares DNs case-insensitively, and so must every join on them.
	dn          TEXT NOT NULL COLLATE NOCASE,
	username    TEXT NOT NULL UNIQUE COLLATE NOCASE,
	name        TEXT NOT NULL,
	email       TEXT NOT NULL DEFAULT '',
	department  TEXT NOT NULL DEFAULT '',
	manager_dn  TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
	is_admin    INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
	-- Set when the account disappears from the directory or is disabled there.
	-- Rows are never deleted: requests reference them for ever.
	disabled    INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
	-- Set by an admin, never by the sync: this person's weekend off runs
	-- Friday to Sunday instead of Saturday to Sunday.
	long_weekend INTEGER NOT NULL DEFAULT 0 CHECK (long_weekend IN (0, 1)),
	synced_at   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_users_dn ON users(dn);
CREATE INDEX IF NOT EXISTS idx_users_manager ON users(manager_dn);

CREATE TABLE IF NOT EXISTS sessions (
	-- sha256 of the cookie value. A read of this table (a backup, a stray
	-- sqlite3 session) yields nothing that can be replayed as a cookie.
	token_hash  TEXT PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id),
	created_at  TEXT NOT NULL,
	expires_at  TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS approver_overrides (
	user_id      INTEGER PRIMARY KEY REFERENCES users(id),
	approver_id  INTEGER NOT NULL REFERENCES users(id),
	CHECK (user_id <> approver_id)
) STRICT;

CREATE TABLE IF NOT EXISTS leave_types (
	id              INTEGER PRIMARY KEY,
	code            TEXT NOT NULL UNIQUE,
	name            TEXT NOT NULL,
	-- NULL means unlimited (unpaid leave): there is no balance to exhaust.
	allowance_days  INTEGER CHECK (allowance_days IS NULL OR allowance_days >= 0),
	cycle_months    INTEGER NOT NULL CHECK (cycle_months IN (12, 36)),
	-- workdays counts the organisation's working days (Admin -> Settings) that are
	-- not public holidays; calendar counts every day (maternity leave is four
	-- consecutive months, holidays included); weekend counts one per weekend
	-- off, which runs Saturday to Sunday, or Friday to Sunday for someone on
	-- long weekends.
	unit            TEXT NOT NULL CHECK (unit IN ('workdays', 'calendar', 'weekend')),
	-- What this type may do with the restricted day (the restricted day in Settings, e.g.
	-- Sunday): any -- no rule; min -- only inside a request of at least
	-- the settings minimum of working days; never -- not at all.
	restricted_rule TEXT NOT NULL DEFAULT 'min' CHECK (restricted_rule IN ('any', 'min', 'never')),
	-- Sick leave is applied for after the fact; nothing else may start in the past.
	backdate        INTEGER NOT NULL DEFAULT 0 CHECK (backdate IN (0, 1)),
	requires_note   INTEGER NOT NULL DEFAULT 0 CHECK (requires_note IN (0, 1)),
	active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	sort            INTEGER NOT NULL DEFAULT 0,
	-- How department coverage limits treat this type. limit: it counts as
	-- someone being away and can be blocked by the limit. count: it counts as
	-- away but is never blocked (nobody should be refused sick leave).
	coverage_rule   TEXT NOT NULL DEFAULT 'limit' CHECK (coverage_rule IN ('limit', 'count'))
) STRICT;

-- The most people of one department that may be away on a single day, as a
-- percentage of the department. department '' is the default for departments
-- with no rule of their own; no row at all means no limit.
CREATE TABLE IF NOT EXISTS coverage_rules (
	department   TEXT PRIMARY KEY,
	max_percent  INTEGER NOT NULL CHECK (max_percent BETWEEN 1 AND 100)
) STRICT;

-- Volunteer teams, as the directory (or later Planning Center) lists them.
-- Rows are never deleted: a team that vanishes from the source is disabled, so
-- its rule survives a brief outage. ext_id is the source's permanent id.
CREATE TABLE IF NOT EXISTS teams (
	id         INTEGER PRIMARY KEY,
	ext_id     TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	source     TEXT NOT NULL CHECK (source IN ('ldap', 'pco', 'fake')),
	disabled   INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
	synced_at  TEXT NOT NULL
) STRICT;

-- Replaced wholesale for a team on each sync. Only people with a row in users
-- are members, so non-staff volunteers are never counted.
CREATE TABLE IF NOT EXISTS team_members (
	team_id  INTEGER NOT NULL REFERENCES teams(id),
	user_id  INTEGER NOT NULL REFERENCES users(id),
	PRIMARY KEY (team_id, user_id)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id);

-- The most of one team that may be away on a single day, as a percentage. No
-- row means the settings default applies, and with no default, no limit.
CREATE TABLE IF NOT EXISTS team_rules (
	team_id      INTEGER PRIMARY KEY REFERENCES teams(id),
	max_percent  INTEGER NOT NULL CHECK (max_percent BETWEEN 1 AND 100)
) STRICT;

-- Policy an admin changes without a deploy: one typed row (id is always 1), not
-- key/value, so STRICT and the CHECKs guard every field. work_days is a
-- comma-separated list of day codes; the Go side validates the list, since
-- SQLite cannot CHECK a set. No final approver means the chain's last step is
-- skipped and the admins take whoever has no manager.
CREATE TABLE IF NOT EXISTS settings (
	id                    INTEGER PRIMARY KEY CHECK (id = 1),
	work_days             TEXT NOT NULL DEFAULT 'mon,tue,wed,thu,fri',
	restricted_day        TEXT NOT NULL DEFAULT '' CHECK (restricted_day IN ('', 'sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat')),
	restricted_min_days   INTEGER NOT NULL DEFAULT 3 CHECK (restricted_min_days BETWEEN 1 AND 31),
	final_approver_id     INTEGER REFERENCES users(id),
	sick_backdate_days    INTEGER NOT NULL DEFAULT 14 CHECK (sick_backdate_days BETWEEN 0 AND 365),
	sick_cycle_start_year INTEGER NOT NULL DEFAULT 2025 CHECK (sick_cycle_start_year BETWEEN 2000 AND 2100),
	-- The default team limit, a percentage; 0 means a team with no rule of its
	-- own has no limit.
	team_default_percent  INTEGER NOT NULL DEFAULT 0 CHECK (team_default_percent BETWEEN 0 AND 100),
	updated_by_id         INTEGER REFERENCES users(id),
	updated_at            TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO settings (id) VALUES (1) ON CONFLICT(id) DO NOTHING;

CREATE TABLE IF NOT EXISTS balance_adjustments (
	id           INTEGER PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id),
	type_id      INTEGER NOT NULL REFERENCES leave_types(id),
	cycle_start  TEXT NOT NULL,
	-- Signed: carry-over and corrections add, a pro-rata start subtracts.
	days         INTEGER NOT NULL CHECK (days <> 0),
	reason       TEXT NOT NULL,
	by_user_id   INTEGER NOT NULL REFERENCES users(id),
	created_at   TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_adjustments_user ON balance_adjustments(user_id, type_id, cycle_start);

CREATE TABLE IF NOT EXISTS holidays (
	date    TEXT PRIMARY KEY,
	name    TEXT NOT NULL,
	source  TEXT NOT NULL CHECK (source IN ('computed', 'manual'))
) STRICT;

-- A company shutdown: a period everyone is off, defined as parts that each
-- charge one leave type (e.g. the first week to annual leave, the rest to an
-- unlimited "company closure" type). Each part becomes a pre-filled draft
-- request per person, which they review and submit.
CREATE TABLE IF NOT EXISTS shutdowns (
	id            INTEGER PRIMARY KEY,
	name          TEXT NOT NULL,
	-- On: a submitted shutdown request is approved at once. Off: it goes
	-- through the person's normal approval chain.
	auto_approve  INTEGER NOT NULL DEFAULT 0 CHECK (auto_approve IN (0, 1)),
	created_by_id INTEGER NOT NULL REFERENCES users(id),
	created_at    TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS shutdown_parts (
	id           INTEGER PRIMARY KEY,
	shutdown_id  INTEGER NOT NULL REFERENCES shutdowns(id) ON DELETE CASCADE,
	type_id      INTEGER NOT NULL REFERENCES leave_types(id),
	start_date   TEXT NOT NULL,
	end_date     TEXT NOT NULL,
	CHECK (end_date >= start_date)
) STRICT;

CREATE TABLE IF NOT EXISTS requests (
	id                  INTEGER PRIMARY KEY,
	user_id             INTEGER NOT NULL REFERENCES users(id),
	type_id             INTEGER NOT NULL REFERENCES leave_types(id),
	-- A cancellation of approved leave is itself a request, approved by the
	-- same chain. It copies the dates of the leave it cancels.
	kind                TEXT NOT NULL CHECK (kind IN ('leave', 'cancellation')),
	cancels_request_id  INTEGER REFERENCES requests(id),
	start_date          TEXT NOT NULL,
	end_date            TEXT NOT NULL,
	days                INTEGER NOT NULL CHECK (days > 0),
	-- The cycle the days count against: the cycle containing start_date.
	cycle_start         TEXT NOT NULL,
	note                TEXT NOT NULL DEFAULT '',
	-- draft: a request waiting for the person to submit it -- a shutdown
	-- request created for them, or one HR allowed to go below zero. Drafts
	-- reserve nothing and block nothing; submitting runs the checks.
	-- awaiting_hr: the person asked to go below a zero balance and an admin
	-- has not yet decided. It counts for overlap and coverage but, like a
	-- draft, reserves no balance.
	status              TEXT NOT NULL CHECK (status IN
	                      ('draft', 'awaiting_hr', 'pending', 'approved', 'declined', 'withdrawn', 'cancelled')),
	-- Set on a request generated for a company shutdown. Its type and dates
	-- are fixed, it may take the balance below zero (the days are mandatory),
	-- and it cannot be withdrawn or cancelled.
	shutdown_part_id    INTEGER REFERENCES shutdown_parts(id),
	-- Resolved and frozen at submit, so a directory change afterwards cannot
	-- leave a pending request with nobody able to act on it. NULL routes the
	-- request to the admins (any admin but the requester): that is where the
	-- final approver's own leave goes, since nobody sits above them.
	approver_id         INTEGER REFERENCES users(id),
	-- Special permission: the request was over a department coverage limit and
	-- needs every level of approval up to the final approver, not just the
	-- first. approver_id moves up one level per approval.
	escalate            INTEGER NOT NULL DEFAULT 0 CHECK (escalate IN (0, 1)),
	-- HR allowed this request to take the balance below zero.
	allow_negative      INTEGER NOT NULL DEFAULT 0 CHECK (allow_negative IN (0, 1)),
	decided_by_id       INTEGER REFERENCES users(id),
	decided_at          TEXT,
	decision_note       TEXT NOT NULL DEFAULT '',
	calendar_event_id   TEXT NOT NULL DEFAULT '',
	created_at          TEXT NOT NULL,
	CHECK (end_date >= start_date),
	CHECK ((kind = 'leave') = (cancels_request_id IS NULL)),
	CHECK (approver_id IS NULL OR user_id <> approver_id)
) STRICT;

CREATE INDEX IF NOT EXISTS idx_requests_user ON requests(user_id, status);
CREATE INDEX IF NOT EXISTS idx_requests_approver ON requests(approver_id, status);
CREATE INDEX IF NOT EXISTS idx_requests_dates ON requests(start_date, end_date);
-- One live request per person per shutdown part. A declined one does not
-- count, so an admin can create the draft again.
CREATE UNIQUE INDEX IF NOT EXISTS idx_requests_one_per_shutdown_part
	ON requests(shutdown_part_id, user_id)
	WHERE shutdown_part_id IS NOT NULL AND status IN ('draft', 'pending', 'approved');
-- At most one live cancellation per leave request.
CREATE UNIQUE INDEX IF NOT EXISTS idx_requests_one_cancellation
	ON requests(cancels_request_id) WHERE status = 'pending';

-- Every decision on an escalated request, and HR's grant or refusal of a
-- negative balance: the trail the request page shows as it moves up.
CREATE TABLE IF NOT EXISTS request_steps (
	id          INTEGER PRIMARY KEY,
	request_id  INTEGER NOT NULL REFERENCES requests(id),
	approver_id INTEGER NOT NULL REFERENCES users(id),
	decision    TEXT NOT NULL CHECK (decision IN ('approved', 'declined', 'granted', 'refused')),
	note        TEXT NOT NULL DEFAULT '',
	decided_at  TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_request_steps_request ON request_steps(request_id);

CREATE TABLE IF NOT EXISTS outbox (
	id               INTEGER PRIMARY KEY,
	kind             TEXT NOT NULL CHECK (kind IN ('email', 'calendar_create', 'calendar_delete')),
	payload          TEXT NOT NULL,
	attempts         INTEGER NOT NULL DEFAULT 0,
	next_attempt_at  TEXT NOT NULL,
	last_error       TEXT NOT NULL DEFAULT '',
	done_at          TEXT,
	-- Set when the job has exhausted its retries; it then shows on the admin
	-- outbox page and waits for a person to retry it.
	failed_at        TEXT,
	created_at       TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox(next_attempt_at) WHERE done_at IS NULL AND failed_at IS NULL;

-- The app is an OAuth authorization server for its own MCP endpoint. Clients
-- register themselves (RFC 7591), so a row here proves nothing about who the
-- client is: it only names it for the consent screen. Ids and tokens are opaque
-- random values; the tables hold only the sha256 of a code or token, as
-- sessions do.
CREATE TABLE IF NOT EXISTS oauth_clients (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	-- A JSON array of the exact redirect URIs the client registered.
	redirect_uris  TEXT NOT NULL,
	created_at     TEXT NOT NULL
) STRICT;

-- One person's consent to one client: what "Connected apps" lists and revokes.
-- Created when a code is exchanged, so abandoned sign-ins leave nothing here.
-- Revoking sets revoked_at, which ends every token of the grant at once.
CREATE TABLE IF NOT EXISTS oauth_grants (
	id            TEXT PRIMARY KEY,
	client_id     TEXT NOT NULL REFERENCES oauth_clients(id),
	user_id       INTEGER NOT NULL REFERENCES users(id),
	scope         TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	last_used_at  TEXT NOT NULL DEFAULT '',
	revoked_at    TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_oauth_grants_user ON oauth_grants(user_id);

-- An authorization code: one use, ten minutes. used_at is set on the first
-- attempt whether or not it succeeds, and a second attempt revokes the grant
-- the first one created (grant_id), as RFC 6749 asks.
CREATE TABLE IF NOT EXISTS oauth_codes (
	code_hash       TEXT PRIMARY KEY,
	client_id       TEXT NOT NULL REFERENCES oauth_clients(id),
	user_id         INTEGER NOT NULL REFERENCES users(id),
	code_challenge  TEXT NOT NULL,
	redirect_uri    TEXT NOT NULL,
	scope           TEXT NOT NULL,
	expires_at      TEXT NOT NULL,
	used_at         TEXT,
	grant_id        TEXT REFERENCES oauth_grants(id) ON DELETE SET NULL
) STRICT;

-- Access tokens (an hour) and refresh tokens (30 days, rotated on every use).
-- A refresh token's used_at marks it spent: presenting it again means two
-- parties hold it, so the whole grant is revoked.
CREATE TABLE IF NOT EXISTS oauth_tokens (
	token_hash  TEXT PRIMARY KEY,
	kind        TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
	grant_id    TEXT NOT NULL REFERENCES oauth_grants(id) ON DELETE CASCADE,
	expires_at  TEXT NOT NULL,
	used_at     TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS idx_oauth_tokens_grant ON oauth_tokens(grant_id);
