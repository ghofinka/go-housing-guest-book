-- PRD C2: regular guests (a maid, a driver, a tutor) an owner has approved once, so the guard can check
-- them in with one tap instead of typing their details every visit.
--
-- The list is the owner's standing consent: it does not expire by itself, and the owner deletes an
-- entry whenever they like. Guards and admins read every house's list (the guard picks the guest at the
-- gate); only the owner of a house writes to it.
CREATE TABLE regular_guests (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    house_id   uuid NOT NULL REFERENCES houses (id),
    guest_name text NOT NULL CHECK (char_length(guest_name) BETWEEN 1 AND 100),
    -- Stored like a visit's plate: upper case, single spaces.
    plate      text CHECK (plate IS NULL OR char_length(plate) BETWEEN 1 AND 12),
    category   text NOT NULL DEFAULT 'tamu' CHECK (category IN ('tamu', 'kurir', 'ojol', 'tukang', 'keluarga', 'lainnya')),
    active     boolean NOT NULL DEFAULT true,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX regular_guests_house_idx ON regular_guests (house_id, created_at DESC, id DESC);
-- The per-house cap counts active entries only.
CREATE INDEX regular_guests_active_idx ON regular_guests (house_id) WHERE active;

GRANT SELECT, INSERT, UPDATE, DELETE ON regular_guests TO guestbook_app;

ALTER TABLE regular_guests ENABLE ROW LEVEL SECURITY;

-- Owners read and write their own house's entries only (WITH CHECK keeps an owner from moving an entry
-- to another house). Guards and admins may read every entry and change none.
CREATE POLICY regular_owner_all ON regular_guests FOR ALL
    USING (current_setting('app.role', true) = 'owner'
           AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid)
    WITH CHECK (current_setting('app.role', true) = 'owner'
                AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);
CREATE POLICY regular_staff_select ON regular_guests FOR SELECT
    USING (current_setting('app.role', true) IN ('guard', 'admin'));

-- A visit made from an entry links to it. Deleting the entry keeps the visit and just drops the link.
ALTER TABLE visits ADD COLUMN regular_guest_id uuid REFERENCES regular_guests (id) ON DELETE SET NULL;
CREATE INDEX visits_regular_guest_idx ON visits (regular_guest_id) WHERE regular_guest_id IS NOT NULL;

-- The owner's approval is standing, so such a visit counts as approved. That fact is stored on the visit
-- itself rather than derived from the link, so it survives the owner deleting the entry: a guest still inside
-- must not drop back to "waiting for an answer" because the list was tidied afterwards.
ALTER TABLE visits ADD COLUMN regular_guest_approved boolean NOT NULL DEFAULT false;
