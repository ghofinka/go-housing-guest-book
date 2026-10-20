-- name: CreateHouse :one
INSERT INTO houses (block, number)
VALUES ($1, $2)
RETURNING id, block, number, active, created_at, updated_at;

-- name: GetHouse :one
SELECT id, block, number, active, created_at, updated_at
FROM houses
WHERE id = $1;

-- name: ListHouses :many
SELECT id, block, number, active, created_at, updated_at
FROM houses
WHERE active OR sqlc.arg('include_inactive')::bool
ORDER BY block, number
LIMIT 1000;

-- name: UpdateHouse :one
UPDATE houses
SET block = COALESCE(sqlc.narg('block'), block),
    number = COALESCE(sqlc.narg('number'), number),
    active = COALESCE(sqlc.narg('active'), active),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id, block, number, active, created_at, updated_at;
