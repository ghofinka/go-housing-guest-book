-- PRD S7: flag guests who are still inside after X hours. X is the admin-editable setting
-- `long_stay_hours` (default 12). The flag itself is computed, never stored.
--
-- Read through a function so every visit query shares one definition, and so a missing or corrupt
-- setting can never make a list query fail: anything that is not a plain 1-4 digit number, or is
-- outside 1..168, falls back to the default.
CREATE FUNCTION long_stay_hours() RETURNS integer
    LANGUAGE sql STABLE
AS $$
    SELECT COALESCE(
        (SELECT value::integer
         FROM settings
         WHERE key = 'long_stay_hours' AND value ~ '^[0-9]{1,4}$' AND value::integer BETWEEN 1 AND 168),
        12)
$$;

GRANT EXECUTE ON FUNCTION long_stay_hours() TO guestbook_app;
