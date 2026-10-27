# Retention, deletion and the privacy notice

How long the guest book keeps photos and personal data (PRD M9), how guests can have their data
deleted, and the public privacy notice (PRD M8). Written for the person who runs the system.

## What is kept, and for how long

| Data | Rule | Default | Setting |
|---|---|---|---|
| Vehicle and face photos | File deleted, photo link removed | **30 days** after check-in | `photo_retention_days` |
| Guest name, plate, purpose note | Blanked (set to empty) | **90 days** after check-in | `personal_data_retention_days` |
| Visit record itself (house, category, entry/exit times, status) | Kept, without the personal fields | kept | - |
| Audit log | Kept (no personal data in it: ids, actions, times) | no limit yet | - |

- Age is counted from the **check-in time**, whatever the status. A guest who was never checked out is
  treated the same, so a forgotten check-out cannot keep data forever.
- Photos may not outlive the personal text: `photo_retention_days` must be <= `personal_data_retention_days`.
- Allowed range: photos 1-365 days, personal data 1-1095 days.
- Shortening a limit deletes data at the next run, and **deleted data cannot be recovered**.

## The daily job

The API runs the job itself every day at **02:00 WIB** (set `RETENTION_RUN_AT=HH:MM`; turn off with
`RETENTION_ENABLED=false` only if you schedule the command below yourself).

A run:
1. takes a database lock, so two runs never overlap (several API instances, or the API and the command);
2. deletes photo files older than the photo limit and clears their paths, in batches. The file is deleted
   **first**; if a file cannot be deleted its path is kept and the next run tries again;
3. blanks the personal fields of records older than the personal-data limit and marks them anonymized;
4. writes a row to `retention_runs` and an audit entry `retention_run`, and logs only counts.

It is safe to run again at any time: a second run finds nothing left to do.

### Run it by hand

**API (admin login):**

```
POST /v1/retention/run?dry_run=true   # report only, changes nothing
POST /v1/retention/run                # do it
```

Returns `photos_deleted`, `records_anonymized`, `files_failed`. `409` means a run is already in progress.

**Command line** (for Windows Task Scheduler or cron; needs `DATABASE_URL` and `PHOTO_DIR`):

```
go run ./cmd/retention -dry-run
go run ./cmd/retention
```

Exit code 0 = finished cleanly. Non-zero = an error, or some photo files could not be deleted
(they are retried next run).

> **First time on real data: always do a dry run first** and check the numbers.

### Check that it is working

`GET /v1/settings/retention` (admin) returns the two limits and the last 10 runs (when, trigger
`schedule`/`manual`/`cli`, counts, and any error). A run with `error` set needs attention. The audit log
(`GET /v1/audit-logs?action=retention_run`) shows who started manual runs.

### Change the limits

```
PUT /v1/settings/retention
{ "photo_retention_days": 30, "personal_data_retention_days": 90 }
```

Both fields are required. The change is audited (old and new values) and applies from the next run. The
public privacy notice shows the new numbers immediately.

## Deleting a guest's data on request

When a guest (or a resident) asks for their entry to be deleted, an admin finds the visit and calls:

```
DELETE /v1/visits/{id}/personal-data
```

Its photos are deleted and its name, plate and note are blanked **immediately**. The record stays, without
them, so counts and times are not disturbed. Repeating the call does nothing more. It is audited as
`visit_erased` with the admin's account. Only admins can do this. If a photo file cannot be deleted the
call fails and nothing is changed, so it can simply be repeated.

Clients can tell an erased or expired visit by `"anonymized": true` and the empty fields.

## The public privacy notice (M8)

- `GET /privacy`: a small web page, no login. **This is the address to put on the gate poster / QR code.**
- `GET /v1/privacy-notice`: the same content as JSON for the app screen.

It says what is recorded, why, who can see it, how long it is kept, the guest's rights and who to contact.
The retention numbers and the face-photo wording are read from the live settings, so the notice always
matches what the system does.

Admins set the contact and an optional note:

```
PUT /v1/settings/privacy
{ "contact": "Pak RT 08, 0812-xxxx-xxxx", "extra_note": "..." }
```

Until a contact is set the notice says "Hubungi pengurus perumahan atau petugas jaga." Set a real one
before go-live.

### Before go-live (not done by the software)

- [ ] Set the privacy contact (`PUT /v1/settings/privacy`).
- [ ] Confirm the retention limits with the pengurus (PRD open question 3) and the face-photo policy
      (`face_photo_required`, open question 2).
- [ ] Print the poster and generate a QR code that points to `https://<your-host>/privacy`; post it at the gate.
- [ ] Do a dry run of the retention job and check `GET /v1/settings/retention`.

## Not covered yet

- Retention of the audit log itself.
- Cleanup of expired login sessions (`refresh_tokens`).
- Personal data of pre-registrations (feature S2 is not built).
