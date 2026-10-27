-- M9 retention and M8 privacy notice.

-- One row per retention job run: the documented record of what was purged and when.
CREATE TABLE retention_runs (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    started_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    trigger            text NOT NULL CHECK (trigger IN ('schedule', 'manual', 'cli')),
    dry_run            boolean NOT NULL DEFAULT false,
    photos_deleted     integer NOT NULL DEFAULT 0,
    records_anonymized integer NOT NULL DEFAULT 0,
    error              text
);

GRANT SELECT, INSERT, UPDATE ON retention_runs TO guestbook_app;

-- Defaults agreed for v1: photos 30 days (already seeded), personal text fields 90 days.
INSERT INTO settings (key, value) VALUES
    ('personal_data_retention_days', '90'),
    ('privacy_contact', ''),
    ('privacy_extra_note', '')
ON CONFLICT (key) DO NOTHING;

-- Keep the daily scans cheap: only rows that still have something to purge are indexed.
CREATE INDEX visits_photos_purge_idx ON visits (checkin_at)
    WHERE vehicle_photo_path IS NOT NULL OR face_photo_path IS NOT NULL;
CREATE INDEX visits_anonymize_idx ON visits (checkin_at)
    WHERE anonymized_at IS NULL;
