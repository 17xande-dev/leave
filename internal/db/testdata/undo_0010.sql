BEGIN IMMEDIATE;
ALTER TABLE sessions DROP COLUMN last_seen;
DELETE FROM goose_db_version WHERE version_id = 10;
COMMIT;
