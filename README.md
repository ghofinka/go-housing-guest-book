# Housing Guest Book

Backend API for a residential cluster's digital guest log (*Buku Tamu Digital Perumahan*).
Guards (satpam) check guests in and out and record a vehicle photo instead of holding their ID
cards. Homeowners see only their own house's visits. Admins manage accounts, houses, retention
and audit.

Status: the MVP backend is implemented and tested. Should/Could items are
not started.

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
- Check-in and check-out times are set by the server, never by the client. Check-out is idempotent.
- List visits with filters (`status`, `house_id`, `from`, `to`) and cursor pagination, newest first.
- House numbers come from master data; unknown or inactive houses are rejected.

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

Should/Could items (for example pre-registration endpoints, notifications, offline sync, admin
2FA). There is no `openapi.yaml` yet.

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
