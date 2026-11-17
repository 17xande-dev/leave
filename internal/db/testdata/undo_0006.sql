BEGIN IMMEDIATE;
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
CREATE TRIGGER requests_default_range AFTER INSERT ON requests
BEGIN
    INSERT INTO request_ranges (request_id, start_date, end_date)
        VALUES (NEW.id, NEW.start_date, NEW.end_date);
END;
CREATE TRIGGER requests_sync_range AFTER UPDATE OF start_date, end_date ON requests
WHEN (SELECT count(*) FROM request_ranges WHERE request_id = NEW.id) <= 1
BEGIN
    UPDATE request_ranges SET start_date = NEW.start_date, end_date = NEW.end_date
        WHERE request_id = NEW.id;
    INSERT INTO request_ranges (request_id, start_date, end_date)
        SELECT NEW.id, NEW.start_date, NEW.end_date
        WHERE NOT EXISTS (SELECT 1 FROM request_ranges WHERE request_id = NEW.id);
END;
DELETE FROM goose_db_version WHERE version_id = 6;
COMMIT;
