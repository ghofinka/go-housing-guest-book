ALTER TABLE visits
    DROP COLUMN IF EXISTS client_checkout_captured_at,
    DROP COLUMN IF EXISTS client_captured_at;
