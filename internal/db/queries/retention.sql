-- name: InsertRetentionRun :one
INSERT INTO retention_runs (trigger, dry_run)
VALUES ($1, $2)
RETURNING id;

-- name: FinishRetentionRun :exec
UPDATE retention_runs
SET finished_at = now(), photos_deleted = $2, records_anonymized = $3, error = $4
WHERE id = $1;

-- name: ListRetentionRuns :many
SELECT id, started_at, finished_at, trigger, dry_run, photos_deleted, records_anonymized, error
FROM retention_runs
ORDER BY id DESC
LIMIT $1;

-- name: ListPhotoPurgeBatch :many
-- Visits older than the cutoff that still hold a photo. SKIP LOCKED lets a concurrent
-- request (for example an erasure) proceed; excluded ids are rows whose file could not be deleted.
SELECT id, vehicle_photo_path, face_photo_path
FROM visits
WHERE checkin_at < sqlc.arg('cutoff')
  AND (vehicle_photo_path IS NOT NULL OR face_photo_path IS NOT NULL)
  AND NOT (id = ANY(sqlc.arg('exclude_ids')::uuid[]))
ORDER BY checkin_at, id
LIMIT sqlc.arg('batch_size')
FOR UPDATE SKIP LOCKED;

-- name: ClearVisitPhotoPaths :exec
UPDATE visits SET vehicle_photo_path = NULL, face_photo_path = NULL WHERE id = $1;

-- name: CountPhotosToPurge :one
SELECT (count(vehicle_photo_path) + count(face_photo_path))::int
FROM visits
WHERE checkin_at < $1;

-- name: AnonymizeVisitBatch :many
-- Blanks the personal text fields; house, category, status and times stay for statistics.
WITH batch AS (
    SELECT old.id FROM visits old
    WHERE old.checkin_at < sqlc.arg('cutoff') AND old.anonymized_at IS NULL
    ORDER BY old.checkin_at, old.id
    LIMIT sqlc.arg('batch_size')
    FOR UPDATE SKIP LOCKED
)
UPDATE visits v
SET guest_name = NULL, vehicle_plate = NULL, purpose_note = NULL, anonymized_at = now()
FROM batch
WHERE v.id = batch.id
RETURNING v.id;

-- name: CountRecordsToAnonymize :one
SELECT count(*)::int FROM visits WHERE checkin_at < $1 AND anonymized_at IS NULL;

-- name: GetVisitErasureState :one
SELECT vehicle_photo_path, face_photo_path, anonymized_at
FROM visits
WHERE id = $1
FOR UPDATE;

-- name: EraseVisit :exec
-- Erasure on request: photos and personal fields go now; the row stays for statistics.
UPDATE visits
SET guest_name = NULL, vehicle_plate = NULL, purpose_note = NULL,
    vehicle_photo_path = NULL, face_photo_path = NULL,
    anonymized_at = COALESCE(anonymized_at, now())
WHERE id = $1;
