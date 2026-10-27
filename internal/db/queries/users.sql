-- name: GetUserByEmail :one
SELECT id, name, email, password_hash, role, house_id, contact, active, created_at, updated_at
FROM users
WHERE email = $1;

-- name: GetUserByID :one
SELECT id, name, email, password_hash, role, house_id, contact, active, created_at, updated_at
FROM users
WHERE id = $1;

-- name: GetPrincipal :one
-- Current role, house and status for authenticating a request (never the token's claims).
SELECT id, role, house_id, active
FROM users
WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (name, email, password_hash, role, house_id, contact)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, name, email, password_hash, role, house_id, contact, active, created_at, updated_at;

-- name: ListUsers :many
SELECT id, name, email, role, house_id, contact, active, created_at, updated_at
FROM users
WHERE (sqlc.narg('role')::text IS NULL OR role = sqlc.narg('role'))
  AND (sqlc.narg('house_id')::uuid IS NULL OR house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) > (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at, id
LIMIT sqlc.arg('page_size');

-- name: UpdateUser :one
UPDATE users
SET name = COALESCE(sqlc.narg('name'), name),
    contact = COALESCE(sqlc.narg('contact'), contact),
    house_id = COALESCE(sqlc.narg('house_id'), house_id),
    active = COALESCE(sqlc.narg('active'), active),
    password_hash = COALESCE(sqlc.narg('password_hash'), password_hash),
    updated_at = now()
WHERE id = sqlc.arg('id')
RETURNING id, name, email, password_hash, role, house_id, contact, active, created_at, updated_at;

-- name: RevokeUserRefreshTokens :exec
UPDATE refresh_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: UpsertAdminUser :one
INSERT INTO users (name, email, password_hash, role)
VALUES ($1, $2, $3, 'admin')
ON CONFLICT (email) DO UPDATE
SET name = EXCLUDED.name,
    password_hash = EXCLUDED.password_hash,
    active = true,
    updated_at = now()
WHERE users.role = 'admin'
RETURNING id;
