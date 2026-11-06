ALTER TABLE visits DROP COLUMN IF EXISTS regular_guest_approved;
DROP INDEX IF EXISTS visits_regular_guest_idx;
ALTER TABLE visits DROP COLUMN IF EXISTS regular_guest_id;
DROP TABLE IF EXISTS regular_guests;
