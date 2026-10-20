CREATE TABLE houses (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    block      text NOT NULL,
    number     text NOT NULL,
    active     boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (block, number)
);

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    role          text NOT NULL CHECK (role IN ('guard', 'owner', 'admin')),
    house_id      uuid REFERENCES houses (id),
    contact       text,
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((role = 'owner') = (house_id IS NOT NULL))
);

CREATE TABLE pre_registrations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    house_id      uuid NOT NULL REFERENCES houses (id),
    guest_name    text NOT NULL,
    expected_date date NOT NULL,
    plate         text,
    code          text NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE visits (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_request_id   uuid UNIQUE,
    house_id            uuid NOT NULL REFERENCES houses (id),
    guest_name          text,
    category            text NOT NULL CHECK (category IN ('tamu', 'kurir', 'ojol', 'tukang', 'keluarga', 'lainnya')),
    purpose_note        text,
    vehicle_plate       text,
    vehicle_photo_path  text,
    face_photo_path     text,
    checkin_at          timestamptz NOT NULL DEFAULT now(),
    checkin_by          uuid NOT NULL REFERENCES users (id),
    checkout_at         timestamptz,
    checkout_by         uuid REFERENCES users (id),
    status              text NOT NULL DEFAULT 'inside' CHECK (status IN ('inside', 'exited')),
    pre_registration_id uuid REFERENCES pre_registrations (id),
    anonymized_at       timestamptz,
    CHECK ((status = 'exited') = (checkout_at IS NOT NULL))
);

CREATE INDEX visits_inside_idx ON visits (checkin_at DESC) WHERE status = 'inside';
CREATE INDEX visits_house_checkin_idx ON visits (house_id, checkin_at DESC);
CREATE INDEX visits_plate_idx ON visits (vehicle_plate);

CREATE TABLE audit_logs (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    uuid REFERENCES users (id),
    action     text NOT NULL,
    target     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO settings (key, value) VALUES
    ('photo_retention_days', '30'),
    ('face_photo_required', 'false');

-- Row-level security: owners only see visits of their own house.
-- The app sets `app.role` and `app.house_id` per transaction (SET LOCAL).
-- Note: RLS does not apply to the table owner or superusers, so the API must
-- connect as a separate non-owner role (see 0002).
ALTER TABLE visits ENABLE ROW LEVEL SECURITY;

CREATE POLICY visits_staff ON visits
    USING (current_setting('app.role', true) IN ('guard', 'admin'))
    WITH CHECK (current_setting('app.role', true) IN ('guard', 'admin'));

CREATE POLICY visits_owner ON visits FOR SELECT
    USING (
        current_setting('app.role', true) = 'owner'
        AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid
    );
