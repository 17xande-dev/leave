-- +goose Up
-- Contract step of 0004 (whose comments call it 0005). Every writer stores its
-- own ranges, so the transitional triggers, the cascade and the legacy event id go.
DROP TRIGGER requests_default_range;
DROP TRIGGER requests_sync_range;
-- Adopt an old-container id into an empty first range.
UPDATE request_ranges SET calendar_event_id = (SELECT r.calendar_event_id FROM requests r WHERE r.id = request_ranges.request_id)
WHERE calendar_event_id = ''
  AND start_date = (SELECT min(x.start_date) FROM request_ranges x WHERE x.request_id = request_ranges.request_id)
  AND (SELECT r.calendar_event_id FROM requests r WHERE r.id = request_ranges.request_id) <> '';
-- Blank legacy ids a range already holds.
UPDATE requests SET calendar_event_id = '' WHERE calendar_event_id <> ''
  AND EXISTS (SELECT 1 FROM request_ranges rr WHERE rr.request_id = requests.id AND rr.calendar_event_id = requests.calendar_event_id);
-- Refuse bad data with a diagnostic constraint name.
CREATE TEMP TABLE _guard_0006 (
  requests_without_range INTEGER NOT NULL CONSTRAINT "0006 refused: a request has no request_ranges row" CHECK (requests_without_range = 0),
  orphan_ranges INTEGER NOT NULL CONSTRAINT "0006 refused: a request_ranges row has no request" CHECK (orphan_ranges = 0),
  span_mismatches INTEGER NOT NULL CONSTRAINT "0006 refused: a request start_date or end_date differs from its ranges" CHECK (span_mismatches = 0),
  touching_ranges INTEGER NOT NULL CONSTRAINT "0006 refused: a request has overlapping or adjacent ranges" CHECK (touching_ranges = 0),
  legacy_event_ids INTEGER NOT NULL CONSTRAINT "0006 refused: requests.calendar_event_id holds an event id no range has" CHECK (legacy_event_ids = 0)
);
INSERT INTO _guard_0006 SELECT
  (SELECT count(*) FROM requests r WHERE NOT EXISTS (SELECT 1 FROM request_ranges rr WHERE rr.request_id = r.id)),
  (SELECT count(*) FROM request_ranges rr WHERE NOT EXISTS (SELECT 1 FROM requests r WHERE r.id = rr.request_id)),
  (SELECT count(*) FROM requests r WHERE r.start_date <> (SELECT min(start_date) FROM request_ranges rr WHERE rr.request_id = r.id)
       OR r.end_date <> (SELECT max(end_date) FROM request_ranges rr WHERE rr.request_id = r.id)),
  (SELECT count(*) FROM request_ranges a JOIN request_ranges b ON b.request_id = a.request_id
       AND b.start_date > a.start_date AND b.start_date <= date(a.end_date, '+1 day')),
  (SELECT count(*) FROM requests WHERE calendar_event_id <> '');
DROP TABLE _guard_0006;
-- A deleter that forgets ranges must fail instead of silently cascading.
CREATE TABLE request_ranges_0006 (
    request_id INTEGER NOT NULL REFERENCES requests(id),
    start_date TEXT NOT NULL,
    end_date TEXT NOT NULL,
    calendar_event_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (request_id, start_date),
    CHECK (end_date >= start_date)
) STRICT;
INSERT INTO request_ranges_0006 (request_id, start_date, end_date, calendar_event_id)
    SELECT request_id, start_date, end_date, calendar_event_id FROM request_ranges;
DROP TABLE request_ranges;
ALTER TABLE request_ranges_0006 RENAME TO request_ranges;
CREATE INDEX idx_request_ranges_dates ON request_ranges(start_date, end_date);
ALTER TABLE requests DROP COLUMN calendar_event_id;

-- +goose Down
-- Lossless: restore the 0005 shape with an empty legacy column appended last.
ALTER TABLE requests ADD COLUMN calendar_event_id TEXT NOT NULL DEFAULT '';
CREATE TABLE request_ranges_0005 (
    request_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    start_date TEXT NOT NULL,
    end_date TEXT NOT NULL,
    calendar_event_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (request_id, start_date),
    CHECK (end_date >= start_date)
) STRICT;
INSERT INTO request_ranges_0005 (request_id, start_date, end_date, calendar_event_id)
    SELECT request_id, start_date, end_date, calendar_event_id FROM request_ranges;
DROP TABLE request_ranges;
ALTER TABLE request_ranges_0005 RENAME TO request_ranges;
CREATE INDEX idx_request_ranges_dates ON request_ranges(start_date, end_date);
-- +goose StatementBegin
CREATE TRIGGER requests_default_range AFTER INSERT ON requests
BEGIN
    INSERT INTO request_ranges (request_id, start_date, end_date)
        VALUES (NEW.id, NEW.start_date, NEW.end_date);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER requests_sync_range AFTER UPDATE OF start_date, end_date ON requests
WHEN (SELECT count(*) FROM request_ranges WHERE request_id = NEW.id) <= 1
BEGIN
    UPDATE request_ranges SET start_date = NEW.start_date, end_date = NEW.end_date
        WHERE request_id = NEW.id;
    INSERT INTO request_ranges (request_id, start_date, end_date)
        SELECT NEW.id, NEW.start_date, NEW.end_date
        WHERE NOT EXISTS (SELECT 1 FROM request_ranges WHERE request_id = NEW.id);
END;
-- +goose StatementEnd
