-- name: CreateVisit :one
-- Returns no row when client_request_id already exists (idempotent retry).
INSERT INTO visits (client_request_id, house_id, guest_name, category, purpose_note, vehicle_plate, checkin_by,
                    vehicle_photo_path, face_photo_path, pre_registration_id, client_captured_at, watchlist_hit)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (client_request_id) DO NOTHING
RETURNING id;

-- name: GetVisit :one
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized,
       -- Owner decision (S4). A visit made from a pre-registration counts as approved until the owner says otherwise.
       COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END)::text AS owner_decision,
       d.decided_at AS owner_decided_at,
       -- Empty while pending (never NULL, so it scans into a plain string).
       COALESCE(CASE WHEN d.decision IS NOT NULL THEN 'owner' WHEN v.pre_registration_id IS NOT NULL THEN 'pre_registration' END, '')::text AS owner_decision_source,
       -- Long stay (S7): still inside after the configured number of hours, by the server clock.
       (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours()))::bool AS overstay,
       v.client_captured_at, v.client_checkout_captured_at,
       v.watchlist_hit
FROM visits v
JOIN houses h ON h.id = v.house_id
LEFT JOIN visit_decisions d ON d.visit_id = v.id
WHERE v.id = $1;

-- name: GetVisitByClientRequestID :one
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized,
       -- Owner decision (S4). A visit made from a pre-registration counts as approved until the owner says otherwise.
       COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END)::text AS owner_decision,
       d.decided_at AS owner_decided_at,
       -- Empty while pending (never NULL, so it scans into a plain string).
       COALESCE(CASE WHEN d.decision IS NOT NULL THEN 'owner' WHEN v.pre_registration_id IS NOT NULL THEN 'pre_registration' END, '')::text AS owner_decision_source,
       -- Long stay (S7): still inside after the configured number of hours, by the server clock.
       (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours()))::bool AS overstay,
       v.client_captured_at, v.client_checkout_captured_at,
       v.watchlist_hit
FROM visits v
JOIN houses h ON h.id = v.house_id
LEFT JOIN visit_decisions d ON d.visit_id = v.id
WHERE v.client_request_id = $1;

-- name: CheckoutVisit :one
-- Only an 'inside' visit changes; the server clock sets checkout_at. The device time, if any,
-- is stored beside it (S5) and, like checkout_at, only the first check-out writes it.
UPDATE visits
SET status = 'exited', checkout_at = now(), checkout_by = $2, client_checkout_captured_at = $3
WHERE id = $1 AND status = 'inside'
RETURNING id;

-- name: ListInsideVisits :many
-- Hot path (guests currently inside): literal status so the partial index applies.
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized,
       -- Owner decision (S4). A visit made from a pre-registration counts as approved until the owner says otherwise.
       COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END)::text AS owner_decision,
       d.decided_at AS owner_decided_at,
       -- Empty while pending (never NULL, so it scans into a plain string).
       COALESCE(CASE WHEN d.decision IS NOT NULL THEN 'owner' WHEN v.pre_registration_id IS NOT NULL THEN 'pre_registration' END, '')::text AS owner_decision_source,
       -- Long stay (S7): still inside after the configured number of hours, by the server clock.
       (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours()))::bool AS overstay,
       v.client_captured_at, v.client_checkout_captured_at,
       v.watchlist_hit
FROM visits v
JOIN houses h ON h.id = v.house_id
LEFT JOIN visit_decisions d ON d.visit_id = v.id
WHERE v.status = 'inside'
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_checkin_at')::timestamptz IS NULL
       OR (v.checkin_at, v.id) < (sqlc.narg('after_checkin_at')::timestamptz, sqlc.narg('after_id')::uuid))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR v.checkin_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR v.checkin_at < sqlc.narg('before_at'))
  AND (sqlc.narg('category')::text IS NULL OR v.category = sqlc.narg('category'))
  AND (sqlc.narg('name_like')::text IS NULL OR v.guest_name ILIKE sqlc.narg('name_like'))
  AND (sqlc.narg('plate_like')::text IS NULL OR replace(v.vehicle_plate, ' ', '') ILIKE sqlc.narg('plate_like'))
  AND (sqlc.narg('decision')::text IS NULL
       OR COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END) = sqlc.narg('decision'))
  AND (sqlc.narg('overstay_only')::bool IS NOT TRUE
       OR (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours())))
ORDER BY v.checkin_at DESC, v.id DESC
LIMIT sqlc.arg('page_size');

-- name: ListVisits :many
SELECT v.id, v.house_id, h.block, h.number, v.guest_name, v.category, v.purpose_note,
       v.vehicle_plate, v.checkin_at, v.checkin_by, v.checkout_at, v.checkout_by, v.status,
       (v.vehicle_photo_path IS NOT NULL)::bool AS has_vehicle_photo, (v.face_photo_path IS NOT NULL)::bool AS has_face_photo,
       (v.anonymized_at IS NOT NULL)::bool AS anonymized,
       -- Owner decision (S4). A visit made from a pre-registration counts as approved until the owner says otherwise.
       COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END)::text AS owner_decision,
       d.decided_at AS owner_decided_at,
       -- Empty while pending (never NULL, so it scans into a plain string).
       COALESCE(CASE WHEN d.decision IS NOT NULL THEN 'owner' WHEN v.pre_registration_id IS NOT NULL THEN 'pre_registration' END, '')::text AS owner_decision_source,
       -- Long stay (S7): still inside after the configured number of hours, by the server clock.
       (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours()))::bool AS overstay,
       v.client_captured_at, v.client_checkout_captured_at,
       v.watchlist_hit
FROM visits v
JOIN houses h ON h.id = v.house_id
LEFT JOIN visit_decisions d ON d.visit_id = v.id
WHERE (sqlc.narg('status')::text IS NULL OR v.status = sqlc.narg('status'))
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('after_checkin_at')::timestamptz IS NULL
       OR (v.checkin_at, v.id) < (sqlc.narg('after_checkin_at')::timestamptz, sqlc.narg('after_id')::uuid))
  AND (sqlc.narg('from_at')::timestamptz IS NULL OR v.checkin_at >= sqlc.narg('from_at'))
  AND (sqlc.narg('before_at')::timestamptz IS NULL OR v.checkin_at < sqlc.narg('before_at'))
  AND (sqlc.narg('category')::text IS NULL OR v.category = sqlc.narg('category'))
  AND (sqlc.narg('name_like')::text IS NULL OR v.guest_name ILIKE sqlc.narg('name_like'))
  AND (sqlc.narg('plate_like')::text IS NULL OR replace(v.vehicle_plate, ' ', '') ILIKE sqlc.narg('plate_like'))
  AND (sqlc.narg('decision')::text IS NULL
       OR COALESCE(d.decision, CASE WHEN v.pre_registration_id IS NOT NULL THEN 'approved' ELSE 'pending' END) = sqlc.narg('decision'))
  AND (sqlc.narg('overstay_only')::bool IS NOT TRUE
       OR (v.status = 'inside' AND v.checkin_at < now() - make_interval(hours => long_stay_hours())))
ORDER BY v.checkin_at DESC, v.id DESC
LIMIT sqlc.arg('page_size');

-- name: CountVisits :one
-- Rows a report would contain (same filters as ListVisits, without paging), so a report can be
-- refused or audited with an exact count before it starts.
SELECT count(*)::int
FROM visits v
WHERE (sqlc.narg('status')::text IS NULL OR v.status = sqlc.narg('status'))
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
  AND (sqlc.narg('category')::text IS NULL OR v.category = sqlc.narg('category'))
  AND v.checkin_at >= sqlc.arg('from_at')
  AND v.checkin_at < sqlc.arg('before_at');

-- name: GetVisitPhotoPaths :one
SELECT vehicle_photo_path, face_photo_path
FROM visits
WHERE id = $1;
