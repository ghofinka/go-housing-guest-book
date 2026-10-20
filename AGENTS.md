# AGENTS.md

Guidance for AI coding agents working in this repo. Product requirements live in [PRD.md](PRD.md) (Indonesian); read it before changing behavior. This file covers the backend: what to build, the stack, and the rules that must not be broken.

## Project

**Housing Guest Book** (Buku Tamu Digital Perumahan): a backend API for a residential cluster's guest log. Guards (satpam) check guests in and out instead of holding their ID cards. Homeowners see only their own house's visits. Admins manage accounts, retention and audit.

Status: greenfield. Only the PRD exists; no code yet. Stack and layout below are the agreed starting point. If you change them, update this file in the same change.

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
4. **Photos are never public (M4, NFR).** Store in private storage; serve only through an authenticated endpoint or short-lived signed URL, after the same house/role check as the visit. Vehicle photo is required when the guest arrives by vehicle; face photo is optional and governed by a cluster policy setting. Compress on upload and validate content type and size.
5. **House numbers come from master data (M1, M10).** `house_id` is a foreign key to an active house; reject unknown or inactive houses. Categories are a fixed enum: `tamu`, `kurir`, `ojol`, `tukang`, `keluarga`, `lainnya`.
6. **Retention (M9).** A scheduled job deletes photos and anonymizes personal fields (guest name, plate) after N days. N is admin-configurable (default photos 30-90 days). The job must be idempotent, logged, and documented. Support deletion on data-subject request, also audited.
7. **Audit log (M11).** Record who viewed, changed or deleted visit/photo/user data (`user_id`, `action`, `target`, `timestamp`). The table is append-only: the application DB role has `INSERT` and `SELECT` only, no `UPDATE`/`DELETE`. Guards and owners cannot read it; admins can.
8. **Check-out (M3).** Idempotent: checking out an already-exited visit returns the existing record, not an error or a second timestamp. "Guests inside" is `status = 'inside'`, indexed, and must load in < 2 s.
9. **Minimal data (privacy).** Do not add personal-data fields beyond the PRD data model without a PRD change. Never log guest names, plates, photo URLs or tokens at info level.
10. **Speed matters (NFR).** Check-in submit < 3 s on normal 4G. Keep the check-in path to one request and one transaction. Avoid N+1 queries on list endpoints and paginate all lists.

## Data model

Follow PRD section 7: `visits`, `houses`, `users`, `pre_registrations`, `audit_logs`. Conventions:

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

Build in PRD order. **MVP = Must Have M1-M11.** Do not start Should/Could items (S1-S8, C1-C7) unless asked, but do not design the schema in a way that blocks them (S2 pre-registration, S3 notifications, S5 offline, S7 long-stay flag). Non-goals: face recognition, ANPR, boom-gate integration, payments, native apps.

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
