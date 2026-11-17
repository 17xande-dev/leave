-- +goose Up
ALTER TABLE leave_types ADD COLUMN staff_can_apply INTEGER NOT NULL DEFAULT 1 CHECK (staff_can_apply IN (0, 1));
-- At the user's request, staff no longer apply for these: admins record or correct them.
UPDATE leave_types SET staff_can_apply = 0 WHERE code IN ('compassionate', 'unpaid');

-- +goose Down
ALTER TABLE leave_types DROP COLUMN staff_can_apply;
