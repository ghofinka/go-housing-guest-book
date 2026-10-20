-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, action, target)
VALUES ($1, $2, $3);
