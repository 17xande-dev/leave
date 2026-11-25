-- +goose Up
ALTER TABLE outbox ADD COLUMN lease_until INTEGER;
ALTER TABLE outbox ADD COLUMN claim_token BLOB CHECK (
    (claim_token IS NULL) = (lease_until IS NULL)
    AND (claim_token IS NULL OR length(claim_token) = 16)
);

-- +goose Down
ALTER TABLE outbox DROP COLUMN claim_token;
ALTER TABLE outbox DROP COLUMN lease_until;
