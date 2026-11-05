-- PRD S3: tell the house owner by email when a guest is recorded.
--
-- A transactional outbox: check-in inserts one row per active owner of the house in the same
-- transaction, and a background worker sends the email later. A check-in that rolls back leaves no
-- row, and a slow or broken mail server never slows the guard down.
--
-- The table holds ids and times only. The owner's address is read from users when the email is sent
-- and the message names no guest, so no personal data of a guest is copied into this table and the
-- retention job has nothing to expire here (the worker deletes old rows itself).
CREATE TABLE notification_outbox (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('visit_arrived')),
    visit_id        uuid NOT NULL REFERENCES visits (id),
    user_id         uuid NOT NULL REFERENCES users (id),
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'skipped')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    -- A worker that claims a row holds it until this time; a crashed worker's claim simply expires.
    locked_until    timestamptz,
    sent_at         timestamptz,
    -- A short reason such as "smtp: 550 mailbox unavailable". Never a guest name or an address.
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Only rows still waiting to go out are scanned by the worker.
CREATE INDEX notification_outbox_due_idx ON notification_outbox (next_attempt_at) WHERE status = 'pending';
CREATE INDEX notification_outbox_visit_idx ON notification_outbox (visit_id);

-- No row-level security: no endpoint reads or writes the table, only the check-in transaction (insert)
-- and the worker (claim, update, delete). The admin settings endpoint reads counts.
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_outbox TO guestbook_app;
