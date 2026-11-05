-- name: CreateWatchlistEntry :one
-- Returns no row when an active entry with the same kind and value already exists.
INSERT INTO watchlist_entries (kind, value, note, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (kind, value) WHERE active DO NOTHING
RETURNING id;

-- name: GetWatchlistEntry :one
SELECT id, kind, value, note, active, created_at, updated_at
FROM watchlist_entries
WHERE id = $1;

-- name: ListWatchlistEntries :many
SELECT id, kind, value, note, active, created_at, updated_at
FROM watchlist_entries
WHERE (sqlc.narg('kind')::text IS NULL OR kind = sqlc.narg('kind'))
  AND (sqlc.narg('active')::bool IS NULL OR active = sqlc.narg('active'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_size');

-- name: UpdateWatchlistEntry :one
-- A field left out (NULL) stays as it is. Fails with a unique violation when reactivating an entry
-- would duplicate an active one.
UPDATE watchlist_entries
SET note       = COALESCE(sqlc.narg('note')::text, note),
    active     = COALESCE(sqlc.narg('active')::bool, active),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id;

-- name: DeleteWatchlistEntry :one
DELETE FROM watchlist_entries WHERE id = $1 RETURNING id;

-- name: WatchlistCheck :one
-- Whether a plate or name matches an active entry, and that entry's note (empty when it has none or
-- there is no match). Goes through a SECURITY DEFINER function so a guard never reads the table itself.
-- The function returns NULL for "no match", so the two columns are made non-null here: sqlc would
-- otherwise generate a plain string that fails to scan a NULL.
SELECT (m.n IS NOT NULL)::bool AS hit, COALESCE(m.n, '')::text AS note
FROM (SELECT watchlist_check(sqlc.narg('plate')::text, sqlc.narg('name')::text) AS n) m;
