-- Guard search (PRD S6): contains-match on guest name and plate must stay fast as the log grows.
-- pg_trgm is a trusted extension, so the migration owner may create it.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX visits_guest_name_trgm_idx ON visits USING gin (guest_name gin_trgm_ops);

-- Plates are stored with single spaces ("B 1234 XYZ") but searched without them ("b1234").
-- The query uses the same expression, so this index is used.
CREATE INDEX visits_plate_nospace_trgm_idx ON visits USING gin ((replace(vehicle_plate, ' ', '')) gin_trgm_ops);
