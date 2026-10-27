-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, action, target)
VALUES ($1, $2, $3);

-- name: ListAuditLogs :many
-- Newest first. Pages on the identity id (monotonic), which the primary key already indexes.
SELECT a.id, a.user_id, u.name AS user_name, a.action, a.target, a.created_at
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
WHERE (sqlc.narg('user_id')::uuid IS NULL OR a.user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action'))
  AND (sqlc.narg('target_prefix')::text IS NULL OR starts_with(a.target, sqlc.narg('target_prefix')))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR a.created_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR a.created_at < sqlc.narg('before_at'))
  AND (sqlc.narg('before_id')::bigint IS NULL OR a.id < sqlc.narg('before_id'))
ORDER BY a.id DESC
LIMIT sqlc.arg('page_size');
