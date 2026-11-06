-- Statistics for a period (PRD C4). Every query filters the same way: check-in time in [from_at, before_at)
-- and, optionally, one house. They return counts only, never a name or plate. Days and hours are in WIB.

-- name: StatsTotals :one
SELECT count(*)::int AS total,
       (count(*) FILTER (WHERE v.status = 'inside'))::int AS still_inside,
       (count(*) FILTER (WHERE v.status = 'inside'
                           AND v.checkin_at < now() - make_interval(hours => long_stay_hours())))::int AS overstay,
       -- How many have left, and the average minutes between check-in and check-out for them. The average is
       -- 0 when nobody has left (never NULL, so it scans into a plain number); `exited` says if it means anything.
       (count(*) FILTER (WHERE v.status = 'exited'))::int AS exited,
       COALESCE(avg(extract(epoch FROM (v.checkout_at - v.checkin_at)) / 60.0) FILTER (WHERE v.status = 'exited'), 0)::float8 AS avg_minutes_inside
FROM visits v
WHERE v.checkin_at >= sqlc.arg('from_at') AND v.checkin_at < sqlc.arg('before_at')
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'));

-- name: StatsByCategory :many
SELECT v.category, count(*)::int AS total
FROM visits v
WHERE v.checkin_at >= sqlc.arg('from_at') AND v.checkin_at < sqlc.arg('before_at')
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
GROUP BY v.category;

-- name: StatsByDay :many
SELECT (v.checkin_at AT TIME ZONE 'Asia/Jakarta')::date AS day, count(*)::int AS total
FROM visits v
WHERE v.checkin_at >= sqlc.arg('from_at') AND v.checkin_at < sqlc.arg('before_at')
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
GROUP BY 1;

-- name: StatsByHour :many
SELECT (extract(hour FROM v.checkin_at AT TIME ZONE 'Asia/Jakarta'))::int AS hour, count(*)::int AS total
FROM visits v
WHERE v.checkin_at >= sqlc.arg('from_at') AND v.checkin_at < sqlc.arg('before_at')
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
GROUP BY 1;

-- name: StatsTopHouses :many
-- The houses with the most visits, most first; ties by block and number so the order is stable.
SELECT h.id, h.block, h.number, count(*)::int AS total
FROM visits v
JOIN houses h ON h.id = v.house_id
WHERE v.checkin_at >= sqlc.arg('from_at') AND v.checkin_at < sqlc.arg('before_at')
  AND (sqlc.narg('house_id')::uuid IS NULL OR v.house_id = sqlc.narg('house_id'))
GROUP BY h.id, h.block, h.number
ORDER BY count(*) DESC, h.block, h.number
LIMIT sqlc.arg('top_n');
