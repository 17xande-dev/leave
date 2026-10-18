// Package db opens the SQLite database and applies the schema.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// Every table is STRICT, so SQLite refuses a string in an INTEGER column
// instead of applying "type affinity" and storing it. STRICT bounds only the
// type class, so the CHECKs carry the rest: a status outside the state machine
// or a boolean of 7 is refused by the database, not just by Go.
//
// Neither STRICT nor a CHECK can be retrofitted: CREATE TABLE IF NOT EXISTS
// does nothing to a table that already exists, and reports success. While this
// is greenfield a schema change means `make recreate-db`, not an ALTER on
// startup -- an ALTER-based helper would leave an old non-strict definition in
// place and silently diverge from this file. Adopt a versioned migration tool
// once there is production data worth keeping.
//
// Dates (start_date, end_date, holidays.date, cycle_start) are TEXT
// YYYY-MM-DD: leave is a matter of calendar days in one timezone, and a
// timestamp would invite an off-by-one at midnight. Instants (created_at,
// expires_at) are RFC3339 TEXT in UTC.
const schema = `
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
	-- workdays counts the organisation's working days (WORK_DAYS) that are
	-- not public holidays; calendar counts every day (maternity leave is four
	-- consecutive months, holidays included); weekend counts one per weekend
	-- off, which runs Saturday to Sunday, or Friday to Sunday for someone on
	-- long weekends.
	unit            TEXT NOT NULL CHECK (unit IN ('workdays', 'calendar', 'weekend')),
	-- What this type may do with the restricted day (RESTRICTED_DAY, e.g.
	-- Sunday): any -- no rule; min -- only inside a request of at least
	-- RESTRICTED_MIN_DAYS working days; never -- not at all.
	restricted_rule TEXT NOT NULL DEFAULT 'min' CHECK (restricted_rule IN ('any', 'min', 'never')),
	-- Sick leave is applied for after the fact; nothing else may start in the past.
	backdate        INTEGER NOT NULL DEFAULT 0 CHECK (backdate IN (0, 1)),
	requires_note   INTEGER NOT NULL DEFAULT 0 CHECK (requires_note IN (0, 1)),
	active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	sort            INTEGER NOT NULL DEFAULT 0
) STRICT;

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
	-- draft: a shutdown request created for the person, not yet submitted.
	-- Drafts reserve nothing and block nothing; submitting runs the checks.
	status              TEXT NOT NULL CHECK (status IN
	                      ('draft', 'pending', 'approved', 'declined', 'withdrawn', 'cancelled')),
	-- Set on a request generated for a company shutdown. Its type and dates
	-- are fixed, it may take the balance below zero (the days are mandatory),
	-- and it cannot be withdrawn or cancelled.
	shutdown_part_id    INTEGER REFERENCES shutdown_parts(id),
	-- Resolved and frozen at submit, so a directory change afterwards cannot
	-- leave a pending request with nobody able to act on it. NULL routes the
	-- request to the admins (any admin but the requester): that is where the
	-- final approver's own leave goes, since nobody sits above them.
	approver_id         INTEGER REFERENCES users(id),
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
`

// Open opens (creating if needed) the database at path and applies the schema.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	// _foreign_keys=on because SQLite enforces no foreign key at all by
	// default. busy_timeout is left at go-sqlite3's 5s default. _txlock=immediate
	// makes every transaction take the write lock at BEGIN, so two concurrent
	// approvals serialise instead of both reading "pending" and one failing
	// with SQLITE_BUSY at commit.
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_txlock=immediate", path)
	d, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Checked before the schema runs: on an old database the new indexes
	// would fail first, with a "no such column" that explains nothing.
	if err := checkCurrent(d); err != nil {
		d.Close()
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return d, nil
}

// checkCurrent refuses a database created by an older build. CREATE TABLE IF
// NOT EXISTS leaves an old table as it was and reports success, so without this
// the first query touching a new column fails mid-request instead of at boot.
// While the app is greenfield the fix is a fresh database, not an ALTER.
func checkCurrent(d *sql.DB) error {
	want := map[string][]string{
		"users":       {"ext_id", "long_weekend"},
		"requests":    {"shutdown_part_id"},
		"leave_types": {"restricted_rule"},
	}
	for table, cols := range want {
		have := map[string]bool{}
		rows, err := d.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			have[n] = true
		}
		rows.Close()
		if len(have) == 0 {
			continue // a fresh database: the schema is about to create it
		}
		for _, c := range cols {
			if !have[c] {
				return fmt.Errorf("the database predates %s.%s: it was created by an older build; "+
					"back it up and start from a fresh one (make recreate-db locally)", table, c)
			}
		}
	}
	// The draft status lives in a CHECK, which pragma_table_info cannot see.
	var ddl string
	err := d.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'requests'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(ddl, "'draft'") {
		return fmt.Errorf("the database predates draft requests; start from a fresh one")
	}
	if err := d.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'leave_types'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(ddl, "'weekend'") {
		return fmt.Errorf("the database predates weekend-off leave types; start from a fresh one")
	}
	return nil
}
