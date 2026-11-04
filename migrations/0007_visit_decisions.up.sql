-- PRD S4: the owner confirms or rejects a visit and the guard sees the answer.
--
-- The decision lives in its own table, not on visits: owners only have SELECT on visits and must not
-- get UPDATE (row-level security cannot limit which columns an update touches, so an owner could then
-- rewrite times or names). Decisions are append-only, like audit_logs: the app role gets SELECT and
-- INSERT only, so the first answer stands and nobody can change or delete it through the API.

-- Lets a decision reference the visit and its house together, so a decision can never be stored
-- for a house that does not own the visit.
ALTER TABLE visits ADD CONSTRAINT visits_id_house_key UNIQUE (id, house_id);

CREATE TABLE visit_decisions (
    visit_id   uuid PRIMARY KEY,
    house_id   uuid NOT NULL,
    decision   text NOT NULL CHECK (decision IN ('approved', 'rejected')),
    decided_by uuid NOT NULL REFERENCES users (id),
    decided_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (visit_id, house_id) REFERENCES visits (id, house_id)
);

GRANT SELECT, INSERT ON visit_decisions TO guestbook_app;

ALTER TABLE visit_decisions ENABLE ROW LEVEL SECURITY;

-- Guards and admins read every decision (the guard needs it at the gate); they cannot write one.
CREATE POLICY visit_decisions_staff_select ON visit_decisions FOR SELECT
    USING (current_setting('app.role', true) IN ('guard', 'admin'));

-- Owners read and write only their own house's decisions.
CREATE POLICY visit_decisions_owner_select ON visit_decisions FOR SELECT
    USING (current_setting('app.role', true) = 'owner'
           AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);
CREATE POLICY visit_decisions_owner_insert ON visit_decisions FOR INSERT
    WITH CHECK (current_setting('app.role', true) = 'owner'
                AND house_id = NULLIF(current_setting('app.house_id', true), '')::uuid);
