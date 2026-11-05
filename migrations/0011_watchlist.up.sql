-- PRD C1: a watchlist of plates and names that need special attention at the gate.
--
-- The list is sensitive, and only an admin may read or change it. A guard still has to learn that a
-- guest matches, so check-in asks through a SECURITY DEFINER function that answers only "hit, and this
-- note". Even a bug in guard-facing code can then not read the list itself: row-level security gives the
-- guard role no rows from the table.
CREATE TABLE watchlist_entries (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       text NOT NULL CHECK (kind IN ('plate', 'name')),
    -- Stored normalized: a plate in upper case without spaces, a name in lower case with single spaces.
    value      text NOT NULL CHECK (char_length(value) BETWEEN 2 AND 100),
    -- A short instruction shown to the guard on a match, for example "Hubungi pengurus".
    note       text NOT NULL DEFAULT '' CHECK (char_length(note) <= 200),
    active     boolean NOT NULL DEFAULT true,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- One active entry per plate or name; a deactivated one can be added again.
CREATE UNIQUE INDEX watchlist_active_value_idx ON watchlist_entries (kind, value) WHERE active;
CREATE INDEX watchlist_created_idx ON watchlist_entries (created_at DESC, id DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON watchlist_entries TO guestbook_app;

ALTER TABLE watchlist_entries ENABLE ROW LEVEL SECURITY;
CREATE POLICY watchlist_admin ON watchlist_entries FOR ALL
    USING (current_setting('app.role', true) = 'admin')
    WITH CHECK (current_setting('app.role', true) = 'admin');

-- Answers whether a plate or a name is on the active list, and returns the entry's note (an empty
-- string when it has none). NULL means no match. It runs with its owner's rights, so it can read the
-- table the caller cannot; it returns the note of one entry and never anything else. The search path is
-- pinned so a caller cannot make it read a look-alike table.
CREATE FUNCTION watchlist_check(p_plate text, p_name text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = public, pg_temp
AS $$
    SELECT e.note
    FROM watchlist_entries e
    WHERE e.active
      AND ((e.kind = 'plate' AND p_plate IS NOT NULL AND e.value = upper(replace(p_plate, ' ', '')))
        OR (e.kind = 'name'  AND p_name  IS NOT NULL AND e.value = lower(regexp_replace(btrim(p_name), '\s+', ' ', 'g'))))
    ORDER BY e.created_at, e.id
    LIMIT 1
$$;

REVOKE ALL ON FUNCTION watchlist_check(text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION watchlist_check(text, text) TO guestbook_app;

-- Whether a visit matched the list when it was recorded. A flag only: the note is shown in the
-- check-in response and is not copied here, so editing an entry later does not rewrite history.
ALTER TABLE visits ADD COLUMN watchlist_hit boolean NOT NULL DEFAULT false;
