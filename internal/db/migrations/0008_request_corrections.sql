-- +goose Up
-- One row per admin correction of a request (Service.Correct), oldest first.
CREATE TABLE request_corrections (
    id           INTEGER PRIMARY KEY,
    request_id   INTEGER NOT NULL REFERENCES requests(id),
    admin_id     INTEGER NOT NULL REFERENCES users(id),
    corrected_at TEXT NOT NULL,
    reason       TEXT NOT NULL CHECK (reason <> ''),
    before_json  TEXT NOT NULL,
    after_json   TEXT NOT NULL
) STRICT;
CREATE INDEX idx_request_corrections_request ON request_corrections(request_id);

-- +goose Down
DROP TABLE request_corrections;
