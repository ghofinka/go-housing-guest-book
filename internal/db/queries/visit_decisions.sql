-- name: InsertVisitDecision :one
-- Decisions are append-only: the first answer stands. Returns no row when the visit was already decided.
INSERT INTO visit_decisions (visit_id, house_id, decision, decided_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (visit_id) DO NOTHING
RETURNING visit_id;

-- name: GetVisitDecision :one
SELECT decision FROM visit_decisions WHERE visit_id = $1;
