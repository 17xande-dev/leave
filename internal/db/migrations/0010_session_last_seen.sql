-- +goose Up
-- A session now ends by an absolute limit counted from created_at and an idle
-- limit counted from last_seen (websec/session), instead of an expires_at that
-- every request pushed forward. last_seen is written at most once a minute.
-- Existing sessions start "seen now", so adding the column signs nobody out;
-- the DEFAULT only satisfies ALTER TABLE, since the app always supplies a value.
-- Same text format as created_at: UTC RFC 3339.
ALTER TABLE sessions ADD COLUMN last_seen TEXT NOT NULL DEFAULT '';
UPDATE sessions SET last_seen = strftime('%Y-%m-%dT%H:%M:%SZ', 'now');

-- +goose Down
ALTER TABLE sessions DROP COLUMN last_seen;
