-- The pg_trgm extension is left installed on purpose: other objects may rely on it.
DROP INDEX IF EXISTS visits_plate_nospace_trgm_idx;
DROP INDEX IF EXISTS visits_guest_name_trgm_idx;
