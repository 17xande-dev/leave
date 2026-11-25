BEGIN IMMEDIATE;
ALTER TABLE outbox DROP COLUMN claim_token;
ALTER TABLE outbox DROP COLUMN lease_until;
DELETE FROM goose_db_version WHERE version_id = 11;
COMMIT;
