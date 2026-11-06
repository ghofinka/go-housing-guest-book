# Could-have checklist (PRD C1-C7)

Branch: `feature/improvements` (from `1bcbf7a`). One commit per item. Last updated 2026-11-06.

`[x]` done and verified, `[ ]` not done, `[ ] (on hold)` waiting for a decision.

## Progress

| Item | What | Done | Status |
|---|---|---|---|
| C1 | Watchlist | 16 / 16 | **Done** and committed |
| C2 | Regular guests | 15 / 15 | **Done** and committed |
| C3 | Plate OCR | 0 / 10 | **On hold**: waiting for your decision on the engine |
| C4 | Statistics dashboard | 13 / 13 | **Done** and committed |
| C5 | Shift handover notes | 15 / 15 | **Done** and committed |
| C6 | Multi-language | 0 / 16 | Not started (done last, so it covers every message above) |
| C7 | Visitor card number | 0 / 12 | Not started |
| | **Total** | **59 / 97** | |

Order: C1, C2, C4, C5, C7, then C6 last. C3 comes in when its engine is decided.

Follow-up ideas that are not part of C1-C7 (soft delete of regular guests, stale inside visits, duplicate detection) are
noted in `docs/IMPROVEMENTS-BACKLOG.md`.

Every item ends with the same closing steps: full suite green, migration up/down/up, docs, `.http`, changelog, commit.

---

## C1 Watchlist (16 / 16)

- [x] C1.1 Migration `0011`: table `watchlist_entries` with row-level security, admin only
- [x] C1.2 `SECURITY DEFINER` function `watchlist_check` (pinned search path, no PUBLIC access), so a guard never reads the list
- [x] C1.3 Column `visits.watchlist_hit`, added to all four visit SELECTs
- [x] C1.4 sqlc queries (create, get, list, update, delete, check) and regenerate
- [x] C1.5 Admin endpoints: create, list (filters, paging), update (note, active), delete
- [x] C1.6 Normalization of plates (no spaces, upper case) and names (lower case, single spaces), same rule in Go and SQL
- [x] C1.7 Check-in integration on every path (plain, fast lane, pre-registration, offline): a flag, never a block; note in the creating response; audit `watchlist_hit`
- [x] C1.8 Hide the flag and note from owners
- [x] C1.9 Wire the handler in `main.go` and the e2e environment
- [x] C1.10 Tests: management and validation, hit at check-in, guard cannot read the list through the database
- [x] C1.11 Negative checks: weakened RLS policy, no space stripping, owner no longer shielded (each made a test fail, then restored)
- [x] C1.12 Full suite green and migration 0011 up/down/up
- [x] C1.13 Docs: `README.md`, `AGENTS.md`, `docs/RETENTION.md`
- [x] C1.14 `.http`: requests for the four watchlist routes (plus a name example); route coverage complete
- [x] C1.15 Change log in `changelog/`
- [x] C1.16 Commit

## C2 Regular guests (15 / 15)

- [x] C2.1 Migration `0012`: table `regular_guests` with row-level security (owner: own house; guard and admin: read all)
- [x] C2.2 Columns `visits.regular_guest_id` and `visits.regular_guest_approved`; the owner-decision CASE extended in all four SELECTs and both filters (approved, source `regular_guest`)
- [x] C2.3 sqlc queries and regenerate
- [x] C2.4 Owner create, update, delete; list by role; limit of 20 active per house
- [x] C2.5 Check-in with `regular_guest_id`: defaults from the entry, link, 422 for an inactive or unknown id
- [x] C2.6 No arrival email for these visits
- [x] C2.7 Audit (created, updated, deleted, listed, used), never the name
- [x] C2.8 Wire the handler in `main.go` and the e2e environment
- [x] C2.9 Tests: CRUD and the limit, cross-house 404, check-in defaults, derived approval, no outbox row, deleted entry keeps old visits approved
- [x] C2.10 Negative checks: email not suppressed, approval not stored, house-blind policy (each made a test fail, then restored)
- [x] C2.11 Full suite green and migration 0012 up/down/up
- [x] C2.12 Docs: `README.md`, `AGENTS.md`, `docs/RETENTION.md`
- [x] C2.13 `.http`: four routes plus a check-in example; route coverage complete
- [x] C2.14 Change log
- [x] C2.15 Commit

## C3 Plate OCR (0 / 10) - ON HOLD

- [ ] (on hold) C3.1 Decide the engine (you are studying this: Tesseract, EasyOCR or PaddleOCR through Python, or an OpenCV pipeline)
- [ ] (on hold) C3.2 `PlateReader` interface
- [ ] (on hold) C3.3 Engine adapter for the chosen engine (subprocess or local sidecar; photos stay on the machine)
- [ ] (on hold) C3.4 Guard endpoint that suggests a plate from a vehicle photo, separate from check-in
- [ ] (on hold) C3.5 Accuracy check on real plate photos
- [ ] (on hold) C3.6 Tests
- [ ] (on hold) C3.7 Docs
- [ ] (on hold) C3.8 `.http`
- [ ] (on hold) C3.9 Change log
- [ ] (on hold) C3.10 Commit

## C4 Statistics dashboard (13 / 13)

- [x] C4.1 SQL aggregate queries (total, by category, by day, by hour, top houses, average stay, still inside, overstay) in WIB
- [x] C4.2 Endpoint `GET /v1/stats/visits` (every role, scoped by role; optional `from`/`to`, default last 7 days; at most 93 days)
- [x] C4.3 Zero-filled days and hours
- [x] C4.4 No audit (aggregates, no personal data, loaded on every login): deviation from the plan
- [x] C4.5 Index `visits(checkin_at)` (migration 0013)
- [x] C4.6 Wire the handler in `main.go` and the e2e environment
- [x] C4.7 Tests: figures, zero-filling, WIB day boundary, totals match the CSV, period validation, admin only
- [x] C4.8 Negative checks (for example UTC bucketing)
- [x] C4.9 Full suite green (and migration check if an index is added)
- [x] C4.10 Docs
- [x] C4.11 `.http`
- [x] C4.12 Change log
- [x] C4.13 Commit

## C5 Shift handover notes (15 / 15)

- [x] C5.1 Migration: table `shift_notes` with row-level security (guard and admin read and insert, admin delete)
- [x] C5.2 sqlc queries and regenerate
- [x] C5.3 Create and list endpoints (guard and admin; newest first, paging, `from`/`to`)
- [x] C5.4 Admin-only delete
- [x] C5.5 Setting `shift_note_retention_days` (default 30, 1-365), optional in `PUT /v1/settings/retention`
- [x] C5.6 Retention job deletes expired notes; counted in `retention_runs` and in the run result and dry run
- [x] C5.7 Audit (created, deleted), never the text
- [x] C5.8 Wire the handler in `main.go` and the e2e environment
- [x] C5.9 Tests: create, list, paging, roles, length, delete, retention of old notes only, backward-compatible PUT
- [x] C5.10 Negative checks
- [x] C5.11 Full suite green and migration up/down/up
- [x] C5.12 Docs, including `docs/RETENTION.md`
- [x] C5.13 `.http`
- [x] C5.14 Change log
- [x] C5.15 Commit

## C6 Multi-language (0 / 16)

- [ ] C6.1 Migration: `users.locale` (`id` or `en`, default `id`)
- [ ] C6.2 `locale` in `POST` and `PATCH /v1/users`
- [ ] C6.3 `PUT /v1/auth/me/locale` for a signed-in user
- [ ] C6.4 Middleware parses `Accept-Language` or `?lang=` once and stores it on the response writer
- [ ] C6.5 Translation table in `internal/i18n`: exact messages plus patterns for messages with numbers
- [ ] C6.6 `httpx.Error` and `ValidationError` translate through that table (no handler changes)
- [ ] C6.7 Go AST test that fails if a message passed to `httpx.Error` or `ValidationError` has no entry
- [ ] C6.8 Privacy notice in English, JSON and the `/privacy` page
- [ ] C6.9 Email templates per language, chosen by the owner's locale
- [ ] C6.10 Tests: precedence, fallback, numbers kept, codes and field names unchanged, notice parity, email language
- [ ] C6.11 Negative checks (remove one table entry and see the completeness test fail)
- [ ] C6.12 Full suite green and migration up/down/up
- [ ] C6.13 Docs
- [ ] C6.14 `.http`
- [ ] C6.15 Change log
- [ ] C6.16 Commit

## C7 Visitor card number (0 / 12)

- [ ] C7.1 Migration: `visits.card_number` and a partial unique index (one card per guest inside)
- [ ] C7.2 `card_number` in the four SELECTs, `CreateVisit` and the list filter; regenerate sqlc
- [ ] C7.3 Check-in field (JSON and multipart) with normalization and validation
- [ ] C7.4 A clash is 422 on `card_number`; a replay is answered before the check
- [ ] C7.5 Visit JSON and the `card_number` list filter
- [ ] C7.6 Tests, including two concurrent check-ins with the same card
- [ ] C7.7 Negative checks (drop the unique index)
- [ ] C7.8 Full suite green and migration up/down/up
- [ ] C7.9 Docs
- [ ] C7.10 `.http`
- [ ] C7.11 Change log
- [ ] C7.12 Commit
