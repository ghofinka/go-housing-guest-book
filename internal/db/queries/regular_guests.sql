-- name: CreateRegularGuest :one
INSERT INTO regular_guests (house_id, guest_name, plate, category, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetRegularGuest :one
SELECT g.id, g.house_id, h.block, h.number, g.guest_name, g.plate, g.category, g.active, g.created_at, g.updated_at
FROM regular_guests g
JOIN houses h ON h.id = g.house_id
WHERE g.id = $1;

-- name: ListRegularGuests :many
SELECT g.id, g.house_id, h.block, h.number, g.guest_name, g.plate, g.category, g.active, g.created_at, g.updated_at
FROM regular_guests g
JOIN houses h ON h.id = g.house_id
WHERE (sqlc.narg('house_id')::uuid IS NULL OR g.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('active')::bool IS NULL OR g.active = sqlc.narg('active'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (g.created_at, g.id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY g.created_at DESC, g.id DESC
LIMIT sqlc.arg('page_size');

-- name: CountActiveRegularGuests :one
SELECT count(*)::int FROM regular_guests WHERE house_id = $1 AND active;

-- name: UpdateRegularGuest :one
-- A field left out (NULL) stays as it is. clear_plate removes the plate, which a plain NULL cannot say.
UPDATE regular_guests
SET guest_name = COALESCE(sqlc.narg('guest_name')::text, guest_name),
    plate      = CASE WHEN sqlc.arg('clear_plate')::bool THEN NULL ELSE COALESCE(sqlc.narg('plate')::text, plate) END,
    category   = COALESCE(sqlc.narg('category')::text, category),
    active     = COALESCE(sqlc.narg('active')::bool, active),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id;

-- name: DeleteRegularGuest :one
DELETE FROM regular_guests WHERE id = $1 RETURNING id;
