DROP INDEX IF EXISTS visits_anonymize_idx;
DROP INDEX IF EXISTS visits_photos_purge_idx;
DELETE FROM settings WHERE key IN ('personal_data_retention_days', 'privacy_contact', 'privacy_extra_note');
DROP TABLE IF EXISTS retention_runs;
