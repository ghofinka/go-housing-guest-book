ALTER TABLE retention_runs DROP COLUMN IF EXISTS shift_notes_deleted;
DELETE FROM settings WHERE key = 'shift_note_retention_days';
DROP TABLE IF EXISTS shift_notes;
