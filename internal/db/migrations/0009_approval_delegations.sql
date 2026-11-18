-- +goose Up
-- An approver who is going away names a stand-in for a date range
-- (Service.Delegate). The request stays with the real approver; while a
-- delegation is active today the stand-in may also decide it. Whether the
-- range is at most 30 days, in the future, and free of another unrevoked
-- delegation of the same person is checked in Go, inside the write
-- transaction.
CREATE TABLE approval_delegations (
    id            INTEGER PRIMARY KEY,
    delegator_id  INTEGER NOT NULL REFERENCES users(id),
    delegate_id   INTEGER NOT NULL REFERENCES users(id),
    start_date    TEXT NOT NULL,
    end_date      TEXT NOT NULL,
    note          TEXT NOT NULL DEFAULT '',
    created_by_id INTEGER NOT NULL REFERENCES users(id),
    created_at    TEXT NOT NULL,
    revoked_by_id INTEGER REFERENCES users(id),
    revoked_at    TEXT,
    CHECK (delegate_id <> delegator_id),
    CHECK (end_date >= start_date),
    CHECK ((revoked_by_id IS NULL) = (revoked_at IS NULL))
) STRICT;
CREATE INDEX idx_delegations_delegator ON approval_delegations(delegator_id, start_date);
CREATE INDEX idx_delegations_delegate  ON approval_delegations(delegate_id, start_date);
-- Who a decision was made for, when the decider acted as a stand-in (NULL
-- otherwise); decided_by_id and approver_id keep the real people.
ALTER TABLE requests ADD COLUMN decided_for_id INTEGER REFERENCES users(id);
ALTER TABLE request_steps ADD COLUMN on_behalf_of_id INTEGER REFERENCES users(id);

-- +goose Down
ALTER TABLE request_steps DROP COLUMN on_behalf_of_id;
ALTER TABLE requests DROP COLUMN decided_for_id;
DROP TABLE approval_delegations;
