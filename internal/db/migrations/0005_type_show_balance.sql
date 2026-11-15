-- +goose Up
ALTER TABLE leave_types ADD COLUMN show_balance INTEGER NOT NULL DEFAULT 1 CHECK (show_balance IN (0, 1));
-- Balances of these four types are HR's business; staff can still apply for them.
UPDATE leave_types SET show_balance = 0 WHERE code IN ('compassionate', 'unpaid', 'parental', 'maternity');

-- +goose Down
ALTER TABLE leave_types DROP COLUMN show_balance;
