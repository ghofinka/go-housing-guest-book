-- PRD C5: shift handover notes. A guard writes a note for the next shift in the app instead of on paper.
--
-- Guards and admins read every note and add new ones; only an admin deletes (a guard cannot rewrite what
-- the previous shift left). A note is free text, so it can hold a name or a plate by accident: it expires
-- after `shift_note_retention_days` (default 30) like the other personal data, deleted by the retention job.
CREATE TABLE shift_notes (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    author_id  uuid NOT NULL REFERENCES users (id),
    body       text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 1000),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX shift_notes_created_idx ON shift_notes (created_at DESC, id DESC);

GRANT SELECT, INSERT, DELETE ON shift_notes TO guestbook_app;

ALTER TABLE shift_notes ENABLE ROW LEVEL SECURITY;

CREATE POLICY shift_notes_staff_select ON shift_notes FOR SELECT
    USING (current_setting('app.role', true) IN ('guard', 'admin'));
CREATE POLICY shift_notes_staff_insert ON shift_notes FOR INSERT
    WITH CHECK (current_setting('app.role', true) IN ('guard', 'admin'));
CREATE POLICY shift_notes_admin_delete ON shift_notes FOR DELETE
    USING (current_setting('app.role', true) = 'admin');

INSERT INTO settings (key, value) VALUES ('shift_note_retention_days', '30') ON CONFLICT (key) DO NOTHING;

-- The retention history counts the notes it deleted.
ALTER TABLE retention_runs ADD COLUMN shift_notes_deleted integer NOT NULL DEFAULT 0;
