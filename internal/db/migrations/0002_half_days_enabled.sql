-- +goose Up
ALTER TABLE settings ADD COLUMN half_days_enabled INTEGER NOT NULL DEFAULT 0 CHECK (half_days_enabled IN (0, 1));

-- +goose Down
ALTER TABLE settings DROP COLUMN half_days_enabled;
