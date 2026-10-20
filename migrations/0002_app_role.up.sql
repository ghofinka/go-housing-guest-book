-- Non-owner role the API connects as, so row-level security is enforced.
-- Dev password only; set a real one out of band in other environments.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'guestbook_app') THEN
        CREATE ROLE guestbook_app LOGIN PASSWORD 'guestbook_app';
    END IF;
END
$$;

GRANT SELECT, INSERT, UPDATE, DELETE ON houses, users, pre_registrations, visits, settings TO guestbook_app;

-- Audit log is append-only: no UPDATE/DELETE for the app role.
GRANT SELECT, INSERT ON audit_logs TO guestbook_app;
