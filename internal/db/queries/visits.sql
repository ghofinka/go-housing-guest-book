-- name: CreateVisit :one
-- Returns no row when client_request_id already exists (idempotent retry).
INSERT INTO visits (client_request_id, house_id, guest_name, category, purpose_note, vehicle_plate, checkin_by,
                    vehicle_photo_path, face_photo_path)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (client_request_id) DO NOTHING
RETURNING id;

-- name: GetVisit :one
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized
FROM visits v
JOIN houses h ON h.id = v.house_id
WHERE v.id = $1;

-- name: GetVisitByClientRequestID :one
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized
FROM visits v
JOIN houses h ON h.id = v.house_id
WHERE v.client_request_id = $1;

-- name: CheckoutVisit :one
-- Only an 'inside' visit changes; the server clock sets checkout_at.
UPDATE visits
SET status = 'exited', checkout_at = now(), checkout_by = $2
WHERE id = $1 AND status = 'inside'
RETURNING id;

-- name: ListInsideVisits :many
-- Hot path (guests currently inside): literal status so the partial index applies.
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized
FROM visits v
JOIN houses h ON h.id = v.house_id
WHERE v.status = 'inside'
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_checkin_at')::timestamptz IS NULL
       OR (v.checkin_at, v.id) < (sqlc.narg('after_checkin_at')::timestamptz, sqlc.narg('after_id')::uuid))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR v.checkin_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR v.checkin_at < sqlc.narg('before_at'))
ORDER BY v.checkin_at DESC, v.id DESC
LIMIT sqlc.arg('page_size');

-- name: ListVisits :many
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized
FROM visits v
JOIN houses h ON h.id = v.house_id
WHERE (sqlc.narg('status')::text IS NULL OR v.status = sqlc.narg('status'))
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_checkin_at')::timestamptz IS NULL
       OR (v.checkin_at, v.id) < (sqlc.narg('after_checkin_at')::timestamptz, sqlc.narg('after_id')::uuid))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR v.checkin_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR v.checkin_at < sqlc.narg('before_at'))
ORDER BY v.checkin_at DESC, v.id DESC
LIMIT sqlc.arg('page_size');

-- name: GetVisitPhotoPaths :one
SELECT vehicle_photo_path, face_photo_path
FROM visits
WHERE id = $1;
