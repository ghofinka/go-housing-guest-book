ALTER TABLE retention_runs DROP COLUMN IF EXISTS preregs_anonymized;

DROP INDEX IF EXISTS visits_pre_registration_idx;

DROP POLICY IF EXISTS prereg_owner_update ON pre_registrations;
DROP POLICY IF EXISTS prereg_owner_insert ON pre_registrations;
DROP POLICY IF EXISTS prereg_owner_select ON pre_registrations;
DROP POLICY IF EXISTS prereg_staff_update ON pre_registrations;
DROP POLICY IF EXISTS prereg_staff_select ON pre_registrations;
ALTER TABLE pre_registrations DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS pre_registrations_anonymize_idx;
DROP INDEX IF EXISTS pre_registrations_house_idx;

ALTER TABLE pre_registrations DROP CONSTRAINT IF EXISTS pre_registrations_used_at_check;
-- Rows whose name was blanked cannot satisfy NOT NULL again; give them an empty name first.
UPDATE pre_registrations SET guest_name = '' WHERE guest_name IS NULL;
ALTER TABLE pre_registrations ALTER COLUMN guest_name SET NOT NULL;
ALTER TABLE pre_registrations
    DROP COLUMN IF EXISTS anonymized_at,
    DROP COLUMN IF EXISTS used_at,
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS created_by;
