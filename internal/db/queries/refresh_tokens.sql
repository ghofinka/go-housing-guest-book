-- name: CreateRefreshToken :one
INSERT INTO refresh_tokens (user_id, family_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: GetRefreshTokenForUpdate :one
SELECT id, user_id, family_id, token_hash, expires_at, revoked_at, replaced_by, created_at
FROM refresh_tokens
WHERE token_hash = $1
FOR UPDATE;

-- name: RevokeRefreshToken :exec
UPDATE refresh_tokens
SET revoked_at = now(), replaced_by = $2
WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshTokenFamily :exec
UPDATE refresh_tokens
SET revoked_at = now()
WHERE family_id = $1 AND revoked_at IS NULL;
