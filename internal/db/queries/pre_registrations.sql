-- name: CreatePreRegistration :one
-- Returns no row when the code is already taken; the caller retries with a new code.
INSERT INTO pre_registrations (house_id, guest_name, expected_date, plate, code, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (code) DO NOTHING
RETURNING id;

-- name: GetPreRegistration :one
SELECT p.id, p.house_id, h.block, h.number, p.guest_name, p.expected_date, p.plate, p.code,
       p.status, p.expires_at, p.created_at, p.used_at, (p.anonymized_at IS NOT NULL)::bool AS anonymized
FROM pre_registrations p
JOIN houses h ON h.id = p.house_id
WHERE p.id = $1;

-- name: GetPreRegistrationByCode :one
SELECT p.id, p.house_id, h.block, h.number, p.guest_name, p.expected_date, p.plate, p.code,
       p.status, p.expires_at, p.created_at, p.used_at, (p.anonymized_at IS NOT NULL)::bool AS anonymized
FROM pre_registrations p
JOIN houses h ON h.id = p.house_id
WHERE p.code = $1;

-- name: ListPreRegistrations :many
SELECT p.id, p.house_id, h.block, h.number, p.guest_name, p.expected_date, p.plate, p.code,
       p.status, p.expires_at, p.created_at, p.used_at, (p.anonymized_at IS NOT NULL)::bool AS anonymized
FROM pre_registrations p
JOIN houses h ON h.id = p.house_id
WHERE (sqlc.narg('status')::text IS NULL OR p.status = sqlc.narg('status'))
  AND (sqlc.narg('house_id')::uuid IS NULL OR p.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (p.created_at, p.id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY p.created_at DESC, p.id DESC
LIMIT sqlc.arg('page_size');

-- name: CountActivePreRegistrations :one
-- Pre-registrations of one house that can still be used (not cancelled, not used, date not past).
SELECT count(*)::int
FROM pre_registrations
WHERE house_id = $1 AND status = 'active' AND expected_date >= $2;

-- name: LockPreRegistrationByCode :one
-- Check-in with a code: the row stays locked until the visit is saved, so a code is used once.
SELECT id, house_id, guest_name, plate, expected_date, status
FROM pre_registrations
WHERE code = $1
FOR UPDATE;

-- name: MarkPreRegistrationUsed :one
UPDATE pre_registrations
SET status = 'used', used_at = now()
WHERE id = $1 AND status = 'active'
RETURNING id;

-- name: CancelPreRegistration :one
-- Cancelling also blanks the personal fields: the guest never came, so nothing needs to be kept.
-- Returns no row when it is not active (already cancelled or used).
UPDATE pre_registrations
SET status = 'cancelled', guest_name = NULL, plate = NULL, anonymized_at = COALESCE(anonymized_at, now())
WHERE id = $1 AND status = 'active'
RETURNING id;
