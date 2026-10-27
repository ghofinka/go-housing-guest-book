-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: GetSettings :many
SELECT key, value, updated_at FROM settings WHERE key = ANY($1::text[]);

-- name: UpsertSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();
