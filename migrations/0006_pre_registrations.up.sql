-- PRD S2: owners pre-register expected guests and hand over a code; the guard uses it at check-in.
-- Until now the table existed but nothing wrote to it, so adding NOT NULL columns is safe. If rows
-- were inserted by hand, this migration fails loudly instead of inventing an owner for them.

ALTER TABLE pre_registrations
    ADD COLUMN created_by    uuid NOT NULL REFERENCES users (id),
    ADD COLUMN status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'used', 'cancelled')),
    ADD COLUMN used_at       timestamptz,
    ADD COLUMN anonymized_at timestamptz,
    -- Name and plate are blanked on cancel and by retention, so the name can no longer be NOT NULL.
    ALTER COLUMN guest_name DROP NOT NULL,
    ADD CONSTRAINT pre_registrations_used_at_check CHECK ((status = 'used') = (used_at IS NOT NULL));

CREATE INDEX pre_registrations_house_idx ON pre_registrations (house_id, created_at DESC, id DESC);
-- The daily retention scan only needs rows that still hold personal data.
CREATE INDEX pre_registrations_anonymize_idx ON pre_registrations (created_at) WHERE anonymized_at IS NULL;

-- Row-level security, same model as visits: the API role is not the table owner, so these apply.
-- Owners see and create only their own house's rows and may update them (cancel). Guards and
-- admins read everything (the guard looks a code up) and update (marking a code used, retention).
-- No policy allows DELETE, so rows are never removed through the API.
ALTER TABLE pre_registrations ENABLE ROW LEVEL SECURITY;

CREATE POLICY prereg_staff_select ON pre_registrations FOR SELECT
    USING (current_setting('app.role', true) IN ('guard', 'admin'));
CREATE POLICY prereg_staff_update ON pre_registrations FOR UPDATE
    USING (current_setting('app.role', true) IN ('guard', 'admin'))
    WITH CHECK (current_setting('app.role', true) IN ('guard', 'admin'));

CREATE POLICY prereg_owner_select ON pre_registrations FOR SELECT
    USING (current_setting('app.role', true) = 'owner'
           AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);
CREATE POLICY prereg_owner_insert ON pre_registrations FOR INSERT
    WITH CHECK (current_setting('app.role', true) = 'owner'
                AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);
CREATE POLICY prereg_owner_update ON pre_registrations FOR UPDATE
    USING (current_setting('app.role', true) = 'owner'
           AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid)
    WITH CHECK (current_setting('app.role', true) = 'owner'
                AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);

-- A visit made from a code points at it; speeds up the join and the "used" lookup.
CREATE INDEX visits_pre_registration_idx ON visits (pre_registration_id) WHERE pre_registration_id IS NOT NULL;

-- Retention also anonymizes pre-registrations; the run record counts them separately.
ALTER TABLE retention_runs ADD COLUMN preregs_anonymized integer NOT NULL DEFAULT 0;
