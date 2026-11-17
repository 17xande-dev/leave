BEGIN IMMEDIATE;
ALTER TABLE leave_types DROP COLUMN staff_can_apply;
DELETE FROM goose_db_version WHERE version_id = 7;
COMMIT;
