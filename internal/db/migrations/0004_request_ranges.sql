-- +goose Up
-- Before 0005's "none remain" guard, blank legacy requests.calendar_event_id
-- values that equal their range's id; about 870 production rows retain them
-- after 0004. Preserve differing legacy ids for the compatibility shim to adopt.
CREATE TABLE request_ranges (
    -- Transitional: old shutdown deletion relies on CASCADE during the swap;
    -- remove it with the compatibility triggers in the 0005 contract step.
    request_id INTEGER NOT NULL REFERENCES requests(id) ON DELETE CASCADE,
    start_date TEXT NOT NULL,
    end_date TEXT NOT NULL,
    calendar_event_id TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (request_id, start_date),
    CHECK (end_date >= start_date)
) STRICT;
INSERT INTO request_ranges (request_id, start_date, end_date, calendar_event_id)
    SELECT id, start_date, end_date, calendar_event_id FROM requests;
CREATE INDEX idx_request_ranges_dates ON request_ranges(start_date, end_date);
-- Transitional: the old container can still insert requests during the swap.
-- insertRanges replaces this default row; 0005 will remove the trigger.
-- +goose StatementBegin
CREATE TRIGGER requests_default_range AFTER INSERT ON requests
BEGIN
    INSERT INTO request_ranges (request_id, start_date, end_date)
        VALUES (NEW.id, NEW.start_date, NEW.end_date);
END;
-- +goose StatementEnd

-- Transitional: an old container's edit must keep its single range in sync.
-- Preserve the event id; multi-range requests are owned by the new binary.
-- Like requests_default_range, this trigger is removed in 0005.
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

-- +goose Down
-- Refuse to silently turn gaps into leave. Production rollback restores backup.
-- +goose StatementBegin
CREATE TEMP TABLE _down_guard (n INTEGER CHECK (n = 0));
INSERT INTO _down_guard SELECT count(*) FROM
    (SELECT request_id FROM request_ranges GROUP BY request_id HAVING count(*) > 1);
DROP TABLE _down_guard;
-- +goose StatementEnd
UPDATE requests SET calendar_event_id = COALESCE(NULLIF((SELECT rr.calendar_event_id
    FROM request_ranges rr WHERE rr.request_id = requests.id), ''), calendar_event_id);
DROP TRIGGER requests_default_range;
DROP TRIGGER requests_sync_range;
DROP TABLE request_ranges;
