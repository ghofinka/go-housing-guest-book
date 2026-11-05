# Housing Guest Book

Backend API for a residential cluster's digital guest log (*Buku Tamu Digital Perumahan*).
Guards (satpam) check guests in and out and record a vehicle photo instead of holding their ID
cards. Homeowners see only their own house's visits. Admins manage accounts, houses, retention
and audit.

Status: the MVP backend is implemented and tested, plus the Should-have items S1 (courier/ojol
fast lane), S2 (pre-registration), S3 (owner email notifications), S4 (owner confirm/reject), S5
(offline sync), S6 (search), S7 (long-stay flag) and S8 (report export), and the Could-have item C1
(watchlist). The other Could-have items are not started.

## Tech stack

| Area | Choice |
|---|---|
| Language | Go 1.26 |
| HTTP | Standard library `net/http` with method/path routing (`POST /v1/visits`) |
| Database | PostgreSQL 16 with Row-Level Security |
| DB access | `pgx/v5` (`pgxpool`); SQL queries generated with `sqlc` |
| Migrations | `golang-migrate` (plain `.sql`, up and down), run as a Go tool |
| Auth | JWT access tokens (`golang-jwt/jwt/v5`) + refresh tokens; argon2id password hashing |
| Logging | `log/slog`, structured, with a request ID |
| Config | Environment variables only (12-factor) |
| Tests | Standard `testing` + `testcontainers-go` against a real Postgres (no DB mocks) |
| Local infra | Docker Compose (`postgres:16-alpine`) |

## Features

**Authentication and roles**
- Login, token refresh, logout, current user (`/v1/auth/me`) and change password.
- Three roles: `guard`, `owner`, `admin`. One account per person.
- Login and password changes are rate limited.

**Visits (guest log)**
- Check-in with house, category, optional guest name, purpose note and vehicle plate.
  Categories: `tamu`, `kurir`, `ojol`, `tukang`, `keluarga`, `lainnya`.
- Check-in is one request: JSON, or `multipart/form-data` with `vehicle_photo` and `face_photo`.
  A vehicle photo is required when the guest arrives by vehicle; the face photo follows the
  cluster's privacy policy setting. A client-generated `client_request_id` makes check-in
  idempotent.
- Courier/ojol fast lane (PRD S1): for `kurir` and `ojol`, only `house_id` and `category` are
  needed. The guest name is optional, the face-photo policy does not apply, and a plate does not
  require a vehicle photo. Name and photos are still stored if the client sends them. Every other
  category keeps the vehicle-photo rule.
- Check-in and check-out times are set by the server, never by the client. Check-out is idempotent.
- List visits with filters (`status`, `house_id`, `from`, `to`) and cursor pagination, newest first.
- Search (PRD S6): `GET /v1/visits` also takes `name` (contains, any case), `plate` (contains,
  ignoring case and spaces) and `category`, at least 2 characters for name and plate. Text is matched
  literally (`%` and `_` are not wildcards) and is never logged. Owners can search too, but only
  within their own house. Anonymized records no longer match by name or plate.
- House numbers come from master data; unknown or inactive houses are rejected.

**Pre-registration (PRD S2)**
- An owner announces an expected guest (`POST /v1/pre-registrations`: `guest_name`, `expected_date`,
  optional `plate`) and gets an 8-character code (no I, O, 0 or 1) to hand over. Clients draw the QR
  from the code; the API does not render images.
- A code is valid only on its expected date (WIB) and works once. At most 20 active
  pre-registrations per house, up to 30 days ahead. Owners list, view and cancel their own
  (`GET /v1/pre-registrations`, `GET /v1/pre-registrations/{id}`, `POST .../{id}/cancel`); cancelling
  blanks the guest name and plate at once. Admins can list all.
- Guards look a code up with `GET /v1/pre-registrations/by-code/{code}` (any case, hyphens and
  spaces ignored; the reply says whether it is still valid) and check in with
  `pre_registration_code` instead of typing the house and name. The house, name and plate come from the
  pre-registration, and the category defaults to `tamu`. A plate still needs its vehicle photo.
- Other houses' pre-registrations answer 404 (row-level security plus a service check). Names and
  plates expire with the personal-data retention limit, see [docs/RETENTION.md](docs/RETENTION.md).

**Watchlist (PRD C1)**
- An admin keeps a list of plates and names that need special attention: `POST/GET /v1/watchlist`,
  `PATCH /v1/watchlist/{id}` (note, active) and `DELETE /v1/watchlist/{id}`. A plate is stored without
  spaces in upper case, a name in lower case with single spaces, and a match is exact on that form. A
  deactivated entry can be added again; `DELETE` removes it for good, so a plate or name can be erased.
- When a guest is checked in with a listed plate or name, the check-in is **recorded as normal** and the
  reply carries `watchlist_hit: true` plus the admin's short note in `watchlist_note`. It never blocks: the
  guard decides at the gate. This applies on every path (plain, courier and ojol, pre-registration, offline).
  The visit keeps `watchlist_hit` for later reads, but not the note, so editing an entry later does not
  rewrite history. Each hit is audited as `watchlist_hit` against the visit.
- The list is readable by **admins only**. A guard learns of a match only through the check-in reply, and
  even the database refuses to show the list to the guard role (row-level security, with a
  `SECURITY DEFINER` function that answers one note and nothing else). Owners never see the flag or the note.
- Every change is audited (`watchlist_created`, `_updated`, `_deleted`, `_listed`) against the entry's id,
  never the plate, name or note. Entries do not expire by themselves; the admin deletes them.

**Report export (PRD S8)**
- Admins download the visit log for a period: `GET /v1/reports/visits.csv` and `GET /v1/reports/visits.pdf`.
  `from` and `to` are required (`YYYY-MM-DD`, WIB, both days included, at most **93 days**). Optional
  filters: `house_id`, `category`, `status`. Newest visits come first.
- Columns: time in and out (WIB), minutes inside, house, category, status, the owner's answer and the
  long-stay flag. **Guest name, plate and note are left out** unless you pass `include_personal=true`, and
  photos and guard names are never included. A record that retention has anonymized has blank personal cells.
- CSV is UTF-8 with a byte-order mark, so Excel opens it correctly, and it is streamed page by page, so a large
  period does not load into memory. Any text cell that starts with `=`, `+`, `-` or `@` (or a tab or carriage
  return) gets a leading apostrophe, so a guest named `=HYPERLINK(...)` cannot run a formula in the admin's
  spreadsheet.
- PDF is A4 landscape with a title, the period, a summary (total, per category, still inside, over the
  long-stay limit) and a table whose header repeats on every page. It is built in memory, so it is limited to
  **5000 rows**; a larger period gets a 422 that suggests the CSV. It uses the standard PDF fonts, so characters
  outside Latin-1 (for example Japanese) show as `?`; the CSV keeps them.
- Every export is recorded in the audit log **before** any data is sent (`visits_exported`: format, period,
  filters, whether personal data was included, and the row count, never a name or plate), so a download that
  stops halfway is still on record. A file that has been downloaded is outside the system: handle exports that
  include personal data with care.
- Browsers on the configured frontend origin can read the file name (`Content-Disposition` is exposed by CORS).

**Owner notifications by email (PRD S3)**
- When a guest is recorded, every active owner of that house is emailed: "Ada tamu (kategori) tiba di
  rumah Anda pukul HH:MM WIB. Buka aplikasi untuk menyetujui atau menolak." The email names **no guest**
  and no plate, only the category and the time. It goes to the owner's login email.
- Courier and ojol visits do not notify unless an admin turns it on: `GET/PUT /v1/settings/notifications`
  (`{"notify_fast_lane": true}`, audited). The same GET shows whether notifications are enabled and how many
  emails are pending, sent, failed or skipped. Pre-registered visits do notify.
- Check-in only writes a row to an outbox in its own transaction, so a slow or broken mail server never
  slows the guard down, and a refused check-in queues nothing. A worker inside the API sends the emails
  every 15 seconds, retrying up to 5 times with backoff (1, 2, 4, 8 minutes) before marking one failed.
  It is safe to run several API instances. Finished rows older than 30 days are deleted.
- It is **off unless SMTP is configured** (see `SMTP_*` below). The public privacy notice then also says
  that owners are emailed. An email already delivered is outside the system and cannot be recalled by
  the retention job.
- Not built: per-owner opt-out (an admin can deactivate the account), delivery receipts, WhatsApp or push.
  The sender is an interface, so another channel can be added without touching check-in.

**Offline sync (PRD S5)**
- The guard post can lose signal, so the client queues the same requests it would send online and
  replays them **oldest first** when it is back. There is no batch endpoint; each request is safe to
  retry because it carries a `client_request_id` (a UUID the client makes up for each check-in).
- Check-in accepts `client_captured_at` (RFC 3339, JSON or multipart): when the device recorded it.
  Photos stay on the normal multipart check-in. A replay answers **201** the first time and **200**
  with the original visit afterwards, so a client can treat 200 as "already synced".
- The server clock stays authoritative: `checkin_at` and `checkout_at` are always the time the server
  received the request, and every filter, sort, retention and long-stay rule uses them. The device time
  is stored beside them and returned as `client_captured_at` and `client_checkout_captured_at`
  (null when the request was made online).
- Plausibility: the device time may be at most 5 minutes ahead of the server and at most 72 hours old,
  otherwise 422 on `client_captured_at` (an older event has to be written in the paper log).
- Check-out works offline too. `POST /v1/visits/by-request/{client_request_id}/checkout` finds the
  visit by the key used at check-in, so the device does not need the server's visit id; replay the
  check-in first (an unknown key is 404). Both check-out routes take an optional body
  `{"client_captured_at": "..."}`. Only the first check-out is stored and audited; a repeat returns the
  visit as it was.
- Known limitation: a replay with the same `client_request_id` but different data returns the first
  visit unchanged, so a client must never reuse a key for a different guest.

**Long-stay flag (PRD S7)**
- Every visit carries `overstay`: true while the guest is still inside after `long_stay_hours` (default
  12), counted from the server's check-in time. `GET /v1/visits?overstay=true` lists them (guards and
  admins see all houses, owners only their own).
- Admins change the limit with `GET/PUT /v1/settings/long-stay` (`{"long_stay_hours": 12}`, 1 to 168).
  It takes effect immediately and is audited with the old and new value. A missing or corrupt stored
  value falls back to 12 so visit lists never fail. The flag is computed, not stored.

**Owner confirm/reject (PRD S4)**
- The owner answers for a guest who is inside: `POST /v1/visits/{id}/decision` with
  `{"decision": "approved"}` or `"rejected"`. Every visit carries `owner_decision`
  (`status` pending, approved or rejected; `decided_at`; `source` owner or pre_registration), so the
  guard sees the answer on the visit and in the inside list. `GET /v1/visits?decision=pending` lists
  what still waits for an answer.
- The first answer stands: repeating it is harmless, a different one is refused (409). No answer after
  the guest has left (409). Another house's visit answers 404, and only owners can answer.
- A visit made from a pre-registration is approved from the start; the owner may still reject it once.
- The answer is advice only: it never blocks check-in or check-out. There is no push notification yet
  (S3), so owners poll `GET /v1/visits?status=inside&decision=pending`.

**Photos**
- JPEG or PNG, up to 10 MB, re-encoded as JPEG on upload.
- Stored on a private local directory (`PHOTO_DIR`), never as a static route. Served only through
  an authenticated endpoint after the same house/role check as the visit.

**Per-house isolation**
- Owners can only read their own house's visits. Another house's record returns `404`, so its
  existence is not leaked. Enforced in the service layer and again by Postgres Row-Level Security.

**Houses and users (admin)**
- Houses master data: list, get, create, update (deactivate instead of delete).
- User management: list, get, create, update.

**Audit log**
- Who viewed, changed or deleted visit, photo or user data. Append-only: the application database
  role can only `INSERT` and `SELECT`. Admins read it at `GET /v1/audit-logs`.

**Retention and privacy**
- Daily job at 02:00 WIB inside the API (also available as `cmd/retention`): photos are deleted
  after 30 days and personal text fields are anonymized after 90 days. Both limits are
  admin-editable. Supports dry runs and records each run.
- Erasure on request: `DELETE /v1/visits/{id}/personal-data` (admin only, audited).
- Public privacy notice at `GET /privacy` and `GET /v1/privacy-notice`; admin settings under
  `/v1/settings/privacy`.
- Operations guide: [docs/RETENTION.md](docs/RETENTION.md).

**Health**
- `GET /healthz` (liveness) and `GET /readyz` (checks the database).

### Not built yet

The Could-have items C2 to C7 and admin 2FA. There is no `openapi.yaml` yet.

## Running the project

### Prerequisites

- [Go](https://go.dev/dl/) 1.26 or newer
- [Docker](https://docs.docker.com/get-docker/) (the local database and the integration tests)
- `make` is optional. On Windows without it, use the PowerShell commands below.

### 1. Configure

```powershell
Copy-Item .env.example .env
```

Edit `.env` and set at least:

| Variable | Notes |
|---|---|
| `DATABASE_URL` | The API connects as the non-owner role `guestbook_app` so row-level security applies. Local default: `postgres://guestbook_app:guestbook_app@localhost:5432/guestbook?sslmode=disable` |
| `MIGRATE_DATABASE_URL` | Migrations and `seed` run as the owner. With the bundled Compose file: `postgres://postgres:postgres@localhost:5432/guestbook?sslmode=disable` |
| `JWT_SECRET` | At least 32 bytes. Generate one: `openssl rand -base64 48`. Never commit it. |

Optional (defaults in `.env.example`): `HTTP_ADDR` (`:8080`), `ACCESS_TOKEN_TTL` (`15m`),
`REFRESH_TOKEN_TTL` (`168h`), `CORS_ORIGIN` (empty = no CORS headers), `PHOTO_DIR`
(`data/photos`), `RETENTION_ENABLED` (`true`), `RETENTION_RUN_AT` (`02:00`, WIB).

Owner email notifications are optional. Leave `SMTP_HOST` empty to keep them off (the API logs one
warning at startup). To turn them on set `SMTP_HOST`, `SMTP_FROM` (for example
`Buku Tamu <noreply@example.com>`), and optionally `SMTP_PORT` (default 587), `SMTP_USERNAME` and
`SMTP_PASSWORD` (set together). Port 465 uses TLS from the start; any other port needs STARTTLS when a
login is configured, because credentials are never sent over a plaintext connection. A half-given
configuration (for example a port without a host) stops the API at startup with a clear message.

`.env` is git-ignored. The Go code reads real environment variables, not the file, so start the
app with `make run` or `scripts/run.ps1`, which load `.env.example` and then `.env` first. A plain
`go run ./cmd/api` will fail with `DATABASE_URL is required`.

### 2. Start Postgres

```powershell
docker compose up -d --wait db     # or: make db-up
```

### 3. Apply migrations

With `make`:

```
make migrate-up
```

Without `make`, load `.env` into the current PowerShell session, then run the migration tool:

```powershell
foreach ($f in ".env.example", ".env") {
  Get-Content $f | Where-Object { $_ -match '^\s*[^#\s][^=]*=' } | ForEach-Object {
    $k, $v = $_ -split '=', 2
    Set-Item "env:$($k.Trim())" $v.Trim().Trim('"')
  }
}
go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate `
  -path migrations -database "$env:MIGRATE_DATABASE_URL" up
```

### 4. Create the first admin

Credentials come from the environment so they are never committed. The password needs at least
10 characters. Running it again for the same email resets that admin's password.

```
make seed        # with ADMIN_EMAIL / ADMIN_PASSWORD set in .env or the shell
```

PowerShell (with `.env` loaded as above):

```powershell
$env:SEED_DATABASE_URL = $env:MIGRATE_DATABASE_URL
$env:ADMIN_EMAIL = "admin@example.com"
$env:ADMIN_PASSWORD = Read-Host "Admin password (min 10 chars)"
go run ./cmd/seed
Remove-Item env:ADMIN_PASSWORD
```

### 5. Run the API

```powershell
make run                    # with make
.\scripts\run.ps1           # Windows, no make
```

The server listens on `HTTP_ADDR` (default `:8080`).

```powershell
curl http://localhost:8080/healthz      # {"status":"ok"}
curl http://localhost:8080/readyz       # {"status":"ready"}

curl -X POST http://localhost:8080/v1/auth/login `
  -H "Content-Type: application/json" `
  -d '{"email":"admin@example.com","password":"<your password>"}'
```

Send the returned access token as `Authorization: Bearer <token>`.

## Development

| Command | What it does |
|---|---|
| `make run` | Run the API |
| `make build` | Build to `bin/api` |
| `make test` | `go test ./...` (needs Docker; starts a real Postgres with testcontainers) |
| `make lint` | `go vet ./...` |
| `make sqlc` | Regenerate `internal/db` from `internal/db/queries` |
| `make migrate-up` / `make migrate-down` | Apply migrations / roll back one |
| `make db-up` / `make db-down` | Start / stop the local Postgres |

Before opening a PR, run `go build ./...`, `go vet ./...` and `go test ./...`.

### Layout

```
cmd/api/          HTTP server
cmd/seed/         create or reset the first admin
cmd/retention/    retention job as a command (cron / Task Scheduler)
internal/         auth, user, house, visit, photo, audit, retention, privacy,
                  httpx (shared helpers), db (sqlc output), server, config
migrations/       NNNN_name.up.sql / .down.sql
scripts/run.ps1   run the API on Windows without make
docs/             operations guides
```

Packages are organized by domain. Handlers call a service; the service calls sqlc queries.

## Conventions

- API errors use one envelope: `{"error": {"code": "...", "message": "..."}}`, with user-facing
  messages in Indonesian. Validation errors return `422` with field-level details.
- Lists are cursor-paginated (`?limit=&cursor=`), newest first.
- Times are stored as `timestamptz` and presented in WIB (`Asia/Jakarta`).
- Never log guest names, plates, photo URLs or tokens at info level, and keep real personal data
  out of the repo, fixtures and logs. Use obviously fake data (`Budi Test`, `B 0000 XX`).

More detail for contributors and AI agents: [AGENTS.md](AGENTS.md).
