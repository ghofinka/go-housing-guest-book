-- name: EnqueueVisitArrived :exec
-- One row per active owner of the visit's house, written in the check-in transaction.
INSERT INTO notification_outbox (kind, visit_id, user_id)
SELECT 'visit_arrived', sqlc.arg('visit_id')::uuid, u.id
FROM users u
WHERE u.house_id = sqlc.arg('house_id')::uuid AND u.role = 'owner' AND u.active;

-- name: ClaimNotifications :many
-- Takes up to batch_size due rows for this worker and hides them from others for two minutes
-- (SKIP LOCKED, so several API instances never pick the same row). The send happens outside any
-- transaction. attempts counts the claim itself, so a worker that crashes mid-send still uses one.
WITH due AS (
    SELECT o.id FROM notification_outbox o
    WHERE o.status = 'pending'
      AND o.next_attempt_at <= now()
      AND (o.locked_until IS NULL OR o.locked_until < now())
    ORDER BY o.id
    LIMIT sqlc.arg('batch_size')
    FOR UPDATE SKIP LOCKED
), claimed AS (
    UPDATE notification_outbox o
    SET locked_until = now() + interval '2 minutes', attempts = o.attempts + 1
    FROM due
    WHERE o.id = due.id
    RETURNING o.id, o.visit_id, o.user_id, o.attempts
)
SELECT c.id, c.attempts, u.email, u.active AS user_active, v.category, v.checkin_at
FROM claimed c
JOIN users u ON u.id = c.user_id
JOIN visits v ON v.id = c.visit_id
ORDER BY c.id;

-- name: MarkNotificationSent :exec
UPDATE notification_outbox
SET status = 'sent', sent_at = now(), locked_until = NULL, last_error = NULL
WHERE id = $1;

-- name: RetryNotificationLater :exec
UPDATE notification_outbox
SET locked_until = NULL, next_attempt_at = $2, last_error = $3
WHERE id = $1;

-- name: MarkNotificationFailed :exec
UPDATE notification_outbox
SET status = 'failed', locked_until = NULL, last_error = $2
WHERE id = $1;

-- name: MarkNotificationSkipped :exec
UPDATE notification_outbox
SET status = 'skipped', locked_until = NULL, last_error = $2
WHERE id = $1;

-- name: DeleteOldNotifications :execrows
-- Finished rows only; a row still pending is never deleted.
DELETE FROM notification_outbox
WHERE status <> 'pending' AND created_at < $1;

-- name: CountNotificationsByStatus :many
SELECT status, count(*)::int AS total FROM notification_outbox GROUP BY status;
