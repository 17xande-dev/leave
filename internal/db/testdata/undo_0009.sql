BEGIN IMMEDIATE;
ALTER TABLE request_steps DROP COLUMN on_behalf_of_id;
ALTER TABLE requests DROP COLUMN decided_for_id;
DROP TABLE approval_delegations;
DELETE FROM goose_db_version WHERE version_id = 9;
COMMIT;
