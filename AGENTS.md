# AGENTS.md

Guidance for AI coding agents working in this repo. Product requirements live in [PRD.md](PRD.md) (Indonesian); read it before changing behavior. This file covers the backend: what to build, the stack, and the rules that must not be broken.

## Project

**Housing Guest Book** (Buku Tamu Digital Perumahan): a backend API for a residential cluster's guest log. Guards (satpam) check guests in and out instead of holding their ID cards. Homeowners see only their own house's visits. Admins manage accounts, retention and audit.

Status: the MVP backend (PRD Must Have M1-M11) is implemented and tested, plus the Should-have items S1 (courier/ojol fast lane), S2 (pre-registration), S3 (owner email notifications), S4 (owner confirm/reject), S5 (offline sync), S6 (search), S7 (long-stay flag) and S8 (report export), plus the Could-have items C1 (watchlist), C2 (regular guests) and C4 (statistics); the other Could-have items are not started. Stack and layout below describe the code as built. If you change them, update this file in the same change. Operations guide for retention, erasure and the privacy notice: [docs/RETENTION.md](docs/RETENTION.md).

## Stack

- Go (latest stable), module path `github.com/ghofinka/go-housing-guest-book` (adjust if the remote differs)
- PostgreSQL 16+
- HTTP: standard library `net/http` with Go 1.22+ method/path routing (`mux.HandleFunc("POST /v1/visits", ...)`)
- DB access: `pgx/v5` with `pgxpool`; queries written in SQL and generated with `sqlc`
- Migrations: `golang-migrate` (plain `.sql` files, up and down)
- Config: environment variables only (12-factor); no secrets in the repo
- Tests: standard `testing` plus `testcontainers-go` for real Postgres. Do not mock the database.

## Layout (planned)

```
cmd/api/            main for the HTTP server
cmd/retention/      main for the scheduled retention job (or a subcommand of api)
internal/
  auth/             login, password hashing, sessions/JWT, RBAC middleware
  house/            houses master data (M10)
  user/             accounts and roles
  visit/            check-in, check-out, history, search (M1-M3, M7, S6)
  photo/            upload, compression, storage, signed URLs (M4)
  prereg/           pre-registration and codes (S2)
  audit/            audit log writer and reader (M11)
  retention/        retention policy and purge logic (M9)
  httpx/            shared handlers helpers, error envelope, middleware
  db/               sqlc output and pgx setup
migrations/         NNNN_name.up.sql / .down.sql
sqlc.yaml
```

Keep packages small and organized by domain, not by layer. Handlers call a service; the service calls sqlc queries.

## Commands (planned; create the Makefile with these targets)

```
make run          # run the API locally
make test         # go test ./... (needs Docker for testcontainers)
make lint         # golangci-lint run + go vet
make sqlc         # regenerate internal/db
make migrate-up   # apply migrations
make migrate-down # roll back one migration
```

Always run `go build ./...`, `go vet ./...` and `go test ./...` before calling work done.

## Domain rules (from the PRD; do not violate)

These are the requirements most likely to be broken by a plausible-looking shortcut.

1. **Server-side timestamps only (M2, M3).** `checkin_at` and `checkout_at` are set by the server (`now()` in the DB or Go), never accepted from the client, never editable by guards. Store as `timestamptz`; present in WIB (`Asia/Jakarta`). Exception: offline sync (S5) may accept a client-captured time, but only as a separate `client_captured_at` field alongside the server's received time.
2. **Per-house access isolation (M5, M6).** A homeowner can only read visits where `house_id` equals their own. Enforce it in two layers: the query/service layer (always filter by the caller's house) and Postgres Row-Level Security as defense in depth. Direct links by ID must return **404**, not 403, for other houses' records so existence is not leaked. Every new endpoint that returns visit or photo data needs a cross-house test.
3. **Roles (M5).** `guard`, `owner`, `admin`. Guards see all visits. Owners see their house only. Admins manage users, houses, retention. One account per person; no shared accounts. Authorization is checked in middleware and again in the service.
4. **Photos are never public (M4, NFR).** Decision (2026-10-21): v1 stores photos on local disk (a configured directory outside any static-file route; `visits.*_photo_path` holds a relative path). Put storage behind a small interface in `internal/photo` so S3-style storage can replace it later without touching handlers. Store in private storage; serve only through an authenticated endpoint or short-lived signed URL, after the same house/role check as the visit. Vehicle photo is required when the guest arrives by vehicle; face photo is optional and governed by a cluster policy setting. Compress on upload and validate content type and size.
5. **House numbers come from master data (M1, M10).** `house_id` is a foreign key to an active house; reject unknown or inactive houses. Categories are a fixed enum: `tamu`, `kurir`, `ojol`, `tukang`, `keluarga`, `lainnya`.
6. **Retention (M9).** Implemented in `internal/retention`. Decided defaults: photos 30 days, personal text fields (guest name, plate, purpose note) anonymized after 90 days, both admin-editable (`settings`), photo limit never longer than the personal-data limit. Age is counted from `checkin_at` regardless of status. The job runs daily at 02:00 WIB inside the API plus `cmd/retention`, takes a Postgres advisory lock, deletes the file **before** clearing its path, is idempotent, supports a dry run, and records each run in `retention_runs` and the audit log (counts only, never personal data). Erasure on request is `DELETE /v1/visits/{id}/personal-data` (admin only, audited). Do not weaken any of this without asking.
7. **Audit log (M11).** Record who viewed, changed or deleted visit/photo/user data (`user_id`, `action`, `target`, `timestamp`). The table is append-only: the application DB role has `INSERT` and `SELECT` only, no `UPDATE`/`DELETE`. Guards and owners cannot read it; admins can.
8. **Check-out (M3).** Idempotent: checking out an already-exited visit returns the existing record, not an error or a second timestamp. "Guests inside" is `status = 'inside'`, indexed, and must load in < 2 s.
9. **Minimal data (privacy).** Do not add personal-data fields beyond the PRD data model without a PRD change. Never log guest names, plates, photo URLs or tokens at info level.
10. **Speed matters (NFR).** Check-in submit < 3 s on normal 4G. Keep the check-in path to one request and one transaction. Avoid N+1 queries on list endpoints and paginate all lists.

## Data model

Follow PRD section 7: `visits`, `houses`, `users`, `pre_registrations`, `audit_logs`; plus `refresh_tokens`, `settings` (retention limits, face-photo policy, privacy contact), `retention_runs` and `visit_decisions` (owner confirm/reject, S4). Conventions:

- `pre_registrations` (S2) has `status` (`active|used|cancelled`), `created_by`, `used_at` and `anonymized_at`, and row-level security like `visits` (owners: own house only; no DELETE policy). A code is valid only on `expected_date` (WIB) and works once. Its guest name and plate are personal data: blanked on cancel and by retention.
- Regular guests (C2, `internal/regular`) are the owner's standing consent: `regular_guests` has row-level security (an owner reads and writes only their own house, guards and admins read all and write nothing), at most 20 active per house, and entries do not expire (the owner deletes them). Check-in accepts `regular_guest_id` (`visit.useRegular`): house, name, plate and category default from the entry, and the visit stores `regular_guest_approved` plus a `regular_guest_id` link that becomes NULL if the entry is deleted. The approval is stored on the visit rather than derived from the link, so deleting an entry never turns a guest who is still inside back into "pending"; `owner_decision` reads approved with source `regular_guest` in every visit SELECT and in the `decision` filters. Do not queue an arrival email for such a visit. Audit by id only (`regular:<id>`), never a name or plate.
- The watchlist (C1, `internal/watchlist`) holds plates and names, so it is admin-only: `watchlist_entries` has row-level security that gives the guard and owner roles no rows, and check-in matches through the `SECURITY DEFINER` function `watchlist_check(plate, name)`, which returns one note (or NULL) and has a pinned `search_path` and no PUBLIC execute right. A match sets `visits.watchlist_hit` and is shown to the guard in the check-in reply; it must never block a check-in or reach an owner (`forCaller` hides it). Plates are stored upper case without spaces and names lower case with single spaces, and the same rule is applied in Go (`watchlist.NormalizePlate`/`NormalizeName`) and in the SQL function. Audit targets are `watchlist:<id>` or `visit:<id>`, never a plate, name or note. Entries do not expire; an admin deletes them (`DELETE` is a real delete, for erasure).
- The report export (S8, `internal/report`) is admin only and reads through `ListVisits`/`CountVisits`; it has no table of its own. It leaves out guest name, plate and note unless `include_personal=true`, never exports photos or guard names, is limited to 93 days (PDF also to 5000 rows), neutralises spreadsheet formulas in every text cell (a leading `'`), and writes one `visits_exported` audit row before any data is sent (format, period, filters, personal flag and row count; never a name or plate). A file that has been downloaded is outside the system and outside retention.
- `notification_outbox` (S3) is a transactional outbox: check-in inserts one row per active owner of the house in its own transaction (only when SMTP is configured; courier/ojol only when the admin setting `notify_fast_lane` is on) and `notify.Worker` sends later, claiming rows with `FOR UPDATE SKIP LOCKED` and sending outside any transaction. It holds ids and times only: the owner's address is read from `users` at send time and the email is a fixed template that names no guest, so no guest data leaves the system. `last_error` is a short summary (for example `smtp reply 550`), never the server's text, which can quote an address. Never put a guest name, plate or address in the email, the outbox, a log line or an audit target.
- Offline sync (S5) keeps rule 1 intact: `visits.client_captured_at` and `visits.client_checkout_captured_at` hold the device's time beside the server's `checkin_at`/`checkout_at`, which stay the receive times. Nothing filters, sorts, expires or flags by the device time. It must be at most 5 minutes ahead and 72 hours old (`visit.ClockSkew`, `visit.MaxOfflineAge`). The window is checked after the idempotent replay, and a repeat check-out is never re-validated.
- The long-stay flag (S7) is computed, never stored: the SQL function `long_stay_hours()` reads the `long_stay_hours` setting (default 12, valid 1-168, anything else falls back to 12) and every visit query returns `overstay` from it, using the server's `checkin_at`.
- `visit_decisions` is append-only like `audit_logs` (the app role has `SELECT, INSERT` only): the owner's first answer stands. It is a separate table because owners have no write access to `visits`. A visit made from a pre-registration is derived as `approved` without a row. The decision is advisory and never blocks check-in or check-out.

- UUID primary keys (`uuid`, generated with `gen_random_uuid()`), except where a human-facing code is needed (pre-registration `code`).
- `created_at`/`updated_at` as `timestamptz`; soft-deactivate houses and users (`active`/`status`) instead of deleting.
- Constraints in the schema, not just in Go: FKs, `CHECK` on category and status, `NOT NULL` on required fields, partial index on `visits(status) WHERE status = 'inside'`, index on `(house_id, checkin_at DESC)`.
- Offline sync (S5) will need client-generated idempotency keys on check-in; keep that in mind when designing the visits table (`client_request_id uuid UNIQUE`).

## API conventions

- JSON over HTTPS, versioned under `/v1`.
- Auth: `Authorization: Bearer <token>`; passwords hashed with argon2id or bcrypt; admin accounts support 2FA (later milestone, plan the schema for it).
- Errors use one envelope: `{"error": {"code": "string_code", "message": "human text in Indonesian"}}` with correct HTTP status codes.
- Validate input at the handler boundary; return 422 with field-level details.
- Pagination: cursor-based (`?limit=&cursor=`), newest first.
- Keep an `openapi.yaml` in sync with the handlers. Contract-level API work (docs, mocks, collection tests) can use the Postman CLI, which is installed on this machine.

## Scope and priority

Build in PRD order. **MVP = Must Have M1-M11.** All Should-have items S1-S8 and the Could-have items C1, C2 and C4 are built. Do not start the other Could-have items (C3-C7) unless asked, but do not design the schema in a way that blocks them (C3 plate OCR, C7 visitor cards). Non-goals: face recognition, ANPR, boom-gate integration, payments, native apps.

## Code style

- `gofmt`/`goimports`; follow Effective Go and the Go code review comments.
- Wrap errors with `%w`; handle every error; no `panic` outside `main`.
- Pass `context.Context` as the first parameter; respect cancellation in DB calls.
- Small interfaces defined where they are consumed. No global state; wire dependencies in `main`.
- Use `log/slog` structured logging with a request ID.
- Code, identifiers, comments and commit messages in English. User-facing API messages in Indonesian (simple wording; the PRD targets non-technical guards).
- Prefer table-driven tests. Name tests for the behavior (`TestOwnerCannotReadOtherHouseVisit`).

## Testing requirements

- Every domain rule above that is enforceable should have a test, especially: cross-house access (UI-equivalent API and direct-ID), server-set timestamps, immutable audit log, retention purge, idempotent check-out.
- Integration tests run against a real Postgres via testcontainers, with migrations applied.
- Do not weaken or delete a failing test to get green; fix the cause or ask.

## Security

- No secrets, tokens or real personal data in the repo, fixtures or logs. Use obviously fake data (`Budi Test`, `B 0000 XX`).
- Parameterized queries only (sqlc/pgx). No string-built SQL.
- Rate-limit login. Set secure headers. Restrict CORS to the configured frontend origin.
- Uploaded files: verify magic bytes, cap size, strip EXIF (GPS) before storing.

## Working agreements

- PRD open questions (section 13) are unresolved: retention days, whether face photo is mandatory, notification channel. Make them configuration, not hard-coded, and ask before choosing a default that changes policy.
- If the PRD and this file conflict, the PRD wins for product behavior; tell the user so this file can be corrected.
- Make small, focused commits. Do not commit unless asked.
