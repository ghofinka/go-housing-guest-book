-- name: CreateShiftNote :one
INSERT INTO shift_notes (author_id, body) VALUES ($1, $2)
RETURNING id;

-- name: GetShiftNote :one
SELECT n.id, n.author_id, u.name AS author_name, n.body, n.created_at
FROM shift_notes n
JOIN users u ON u.id = n.author_id
WHERE n.id = $1;

-- name: ListShiftNotes :many
-- Newest first. from_at/before_at bound the creation time (both optional).
SELECT n.id, n.author_id, u.name AS author_name, n.body, n.created_at
FROM shift_notes n
JOIN users u ON u.id = n.author_id
WHERE (sqlc.narg('from_at')::timestamptz IS NULL OR n.created_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR n.created_at < sqlc.narg('before_at'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (n.created_at, n.id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY n.created_at DESC, n.id DESC
LIMIT sqlc.arg('page_size');

-- name: DeleteShiftNote :one
DELETE FROM shift_notes WHERE id = $1 RETURNING id;

-- name: DeleteExpiredShiftNotesBatch :many
-- Retention: notes older than the cutoff, in batches. No FOR UPDATE here: it would need the UPDATE
-- privilege, which the table does not grant, and the DELETE locks its rows anyway.
WITH batch AS (
    SELECT old.id FROM shift_notes old
    WHERE old.created_at < sqlc.arg('cutoff')
    ORDER BY old.created_at, old.id
    LIMIT sqlc.arg('batch_size')
)
DELETE FROM shift_notes n USING batch WHERE n.id = batch.id
RETURNING n.id;

-- name: CountShiftNotesToDelete :one
SELECT count(*)::int FROM shift_notes WHERE created_at < $1;
