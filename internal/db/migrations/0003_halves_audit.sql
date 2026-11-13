-- +goose Up
ALTER TABLE requests ADD COLUMN halves_set_by_id INTEGER REFERENCES users(id);
ALTER TABLE requests ADD COLUMN halves_set_at TEXT;

-- +goose Down
ALTER TABLE requests DROP COLUMN halves_set_at;
ALTER TABLE requests DROP COLUMN halves_set_by_id;
