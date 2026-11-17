BEGIN IMMEDIATE;
DROP TABLE request_corrections;
DELETE FROM goose_db_version WHERE version_id = 8;
COMMIT;
