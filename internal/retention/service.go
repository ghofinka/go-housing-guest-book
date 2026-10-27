// Package retention enforces how long photos and personal data are kept (M9).
//
// A run deletes photo files and clears photo paths for visits older than the photo
// limit, and blanks the personal text fields (guest name, plate, purpose note) of
// visits older than the personal-data limit. Age alone decides, whatever the
// visit's status, so a forgotten check-out cannot keep data forever. Visit rows,
// houses, categories and times are kept for statistics.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/photo"
)

const (
	SettingPhotoDays    = "photo_retention_days"
	SettingPersonalDays = "personal_data_retention_days"

	defaultPhotoDays    = 30
	defaultPersonalDays = 90

	// Bounds for admin-set limits.
	MinDays         = 1
	MaxPhotoDays    = 365
	MaxPersonalDays = 1095

	batchSize = 200
	// LockKey is the Postgres advisory lock that keeps two runs (two API
	// instances, or the API and the CLI) from overlapping.
	LockKey int64 = 7262026001
)

var ErrAlreadyRunning = errors.New("a retention run is already in progress")

type Trigger string

const (
	TriggerSchedule Trigger = "schedule"
	TriggerManual   Trigger = "manual"
	TriggerCLI      Trigger = "cli"
)

// Settings are the two retention limits, in days.
type Settings struct {
	PhotoDays        int `json:"photo_retention_days"`
	PersonalDataDays int `json:"personal_data_retention_days"`
}

// Validate returns field-level problems (field name -> message). A photo must not
// outlive the personal text of its record, so the photo limit may not exceed the other.
func (s Settings) Validate() map[string]string {
	f := map[string]string{}
	if s.PhotoDays < MinDays || s.PhotoDays > MaxPhotoDays {
		f[SettingPhotoDays] = fmt.Sprintf("Harus antara %d dan %d hari.", MinDays, MaxPhotoDays)
	}
	if s.PersonalDataDays < MinDays || s.PersonalDataDays > MaxPersonalDays {
		f[SettingPersonalDays] = fmt.Sprintf("Harus antara %d dan %d hari.", MinDays, MaxPersonalDays)
	}
	if len(f) == 0 && s.PhotoDays > s.PersonalDataDays {
		f[SettingPhotoDays] = "Masa simpan foto tidak boleh lebih lama dari masa simpan data pribadi."
	}
	return f
}

// Result reports one run. In a dry run nothing was changed and the counts are what would be.
type Result struct {
	RunID             int64 `json:"run_id"`
	DryRun            bool  `json:"dry_run"`
	PhotosDeleted     int   `json:"photos_deleted"`
	RecordsAnonymized int   `json:"records_anonymized"`
	// FilesFailed counts visits whose photo file could not be deleted; they keep
	// their photo path and are retried by the next run.
	FilesFailed int `json:"files_failed"`
}

// RunInfo is one row of the run history.
type RunInfo struct {
	ID                int64      `json:"id"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	Trigger           string     `json:"trigger"`
	DryRun            bool       `json:"dry_run"`
	PhotosDeleted     int        `json:"photos_deleted"`
	RecordsAnonymized int        `json:"records_anonymized"`
	Error             *string    `json:"error"`
}

type Service struct {
	pool   *pgxpool.Pool
	photos photo.Store
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, photos photo.Store) *Service {
	return &Service{pool: pool, photos: photos, now: time.Now}
}

// Settings returns the current limits. Missing rows fall back to the defaults; a
// row that is not a valid number is an error, so a corrupt value can never cause a purge.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	return loadSettings(ctx, db.New(s.pool))
}

func loadSettings(ctx context.Context, q *db.Queries) (Settings, error) {
	rows, err := q.GetSettings(ctx, []string{SettingPhotoDays, SettingPersonalDays})
	if err != nil {
		return Settings{}, fmt.Errorf("read retention settings: %w", err)
	}
	return SettingsFromRows(rows)
}

// SettingsFromRows picks the retention limits out of settings rows (other keys are
// ignored), applying the defaults for missing ones and validating the result.
func SettingsFromRows(rows []db.Setting) (Settings, error) {
	out := Settings{PhotoDays: defaultPhotoDays, PersonalDataDays: defaultPersonalDays}
	for _, r := range rows {
		var field *int
		switch r.Key {
		case SettingPhotoDays:
			field = &out.PhotoDays
		case SettingPersonalDays:
			field = &out.PersonalDataDays
		default:
			continue // not a retention setting
		}
		n, err := strconv.Atoi(r.Value)
		if err != nil {
			return Settings{}, fmt.Errorf("setting %s is not a number", r.Key)
		}
		*field = n
	}
	if problems := out.Validate(); len(problems) > 0 {
		return Settings{}, fmt.Errorf("stored retention settings are invalid: %v", problems)
	}
	return out, nil
}

// UpdateSettings stores new limits (already validated by the caller) and audits the change.
func (s *Service) UpdateSettings(ctx context.Context, actor uuid.UUID, in Settings) (Settings, error) {
	err := db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
		old, err := loadSettings(ctx, q)
		if err != nil {
			return err
		}
		for key, v := range map[string]int{SettingPhotoDays: in.PhotoDays, SettingPersonalDays: in.PersonalDataDays} {
			if err := q.UpsertSetting(ctx, db.UpsertSettingParams{Key: key, Value: strconv.Itoa(v)}); err != nil {
				return fmt.Errorf("save %s: %w", key, err)
			}
		}
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true},
			Action: "retention_settings_changed",
			Target: fmt.Sprintf("%s:%d->%d,%s:%d->%d", SettingPhotoDays, old.PhotoDays, in.PhotoDays,
				SettingPersonalDays, old.PersonalDataDays, in.PersonalDataDays),
		})
	})
	if err != nil {
		return Settings{}, fmt.Errorf("update retention settings: %w", err)
	}
	return in, nil
}

// Runs returns the most recent runs, newest first.
func (s *Service) Runs(ctx context.Context, limit int) ([]RunInfo, error) {
	rows, err := db.New(s.pool).ListRetentionRuns(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("list retention runs: %w", err)
	}
	out := make([]RunInfo, 0, len(rows))
	for _, r := range rows {
		ri := RunInfo{
			ID: r.ID, StartedAt: r.StartedAt.In(httpx.WIB), Trigger: r.Trigger, DryRun: r.DryRun,
			PhotosDeleted: int(r.PhotosDeleted), RecordsAnonymized: int(r.RecordsAnonymized),
		}
		if r.FinishedAt.Valid {
			t := r.FinishedAt.Time.In(httpx.WIB)
			ri.FinishedAt = &t
		}
		if r.Error.Valid {
			ri.Error = &r.Error.String
		}
		out = append(out, ri)
	}
	return out, nil
}

// Run performs one retention pass. actor is the admin who triggered a manual run
// (nil for the scheduler and the CLI). It returns ErrAlreadyRunning if another run holds the lock.
// Only counts are logged, never names, plates or file paths.
func (s *Service) Run(ctx context.Context, trigger Trigger, actor *uuid.UUID, dryRun bool) (Result, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, LockKey).Scan(&locked); err != nil {
		return Result{}, fmt.Errorf("take retention lock: %w", err)
	}
	if !locked {
		return Result{}, ErrAlreadyRunning
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, LockKey) }()

	cfg, err := s.Settings(ctx)
	if err != nil {
		return Result{}, err
	}
	runID, err := db.New(s.pool).InsertRetentionRun(ctx, db.InsertRetentionRunParams{Trigger: string(trigger), DryRun: dryRun})
	if err != nil {
		return Result{}, fmt.Errorf("record run start: %w", err)
	}
	res := Result{RunID: runID, DryRun: dryRun}

	now := s.now()
	photoCutoff := now.AddDate(0, 0, -cfg.PhotoDays)
	personalCutoff := now.AddDate(0, 0, -cfg.PersonalDataDays)

	runErr := s.work(ctx, &res, photoCutoff, personalCutoff)
	s.finish(ctx, trigger, actor, res, runErr)
	slog.Info("retention run finished", "run_id", runID, "trigger", string(trigger), "dry_run", dryRun,
		"photos_deleted", res.PhotosDeleted, "records_anonymized", res.RecordsAnonymized,
		"files_failed", res.FilesFailed, "failed", runErr != nil)
	return res, runErr
}

func (s *Service) work(ctx context.Context, res *Result, photoCutoff, personalCutoff time.Time) error {
	if res.DryRun {
		return db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
			photos, err := q.CountPhotosToPurge(ctx, photoCutoff)
			if err != nil {
				return err
			}
			records, err := q.CountRecordsToAnonymize(ctx, personalCutoff)
			res.PhotosDeleted, res.RecordsAnonymized = int(photos), int(records)
			return err
		})
	}
	if err := s.purgePhotos(ctx, res, photoCutoff); err != nil {
		return fmt.Errorf("purge photos: %w", err)
	}
	if err := s.anonymize(ctx, res, personalCutoff); err != nil {
		return fmt.Errorf("anonymize records: %w", err)
	}
	return nil
}

// purgePhotos deletes the files first and clears the paths in the same transaction,
// so a failure can never leave a file that the database has forgotten about.
func (s *Service) purgePhotos(ctx context.Context, res *Result, cutoff time.Time) error {
	excluded := []uuid.UUID{} // visits whose file could not be deleted this run
	for {
		var rows []db.ListPhotoPurgeBatchRow
		var deleted, failed []int
		err := db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
			var err error
			rows, err = q.ListPhotoPurgeBatch(ctx, db.ListPhotoPurgeBatchParams{
				Cutoff: cutoff, ExcludeIds: excluded, BatchSize: batchSize})
			if err != nil {
				return err
			}
			deleted, failed = deleted[:0], failed[:0]
			for i, r := range rows {
				files, ok := 0, true
				for _, p := range []pgtype.Text{r.VehiclePhotoPath, r.FacePhotoPath} {
					if !p.Valid {
						continue
					}
					if err := s.photos.Delete(ctx, p.String); err != nil {
						slog.Error("retention: delete photo file", "visit_id", r.ID.String(), "err", err)
						ok = false
						break
					}
					files++
				}
				if !ok {
					failed = append(failed, i)
					continue
				}
				if err := q.ClearVisitPhotoPaths(ctx, r.ID); err != nil {
					return err
				}
				deleted = append(deleted, files)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, n := range deleted {
			res.PhotosDeleted += n
		}
		for _, i := range failed {
			res.FilesFailed++
			excluded = append(excluded, rows[i].ID)
		}
		if len(rows) == 0 {
			return nil
		}
	}
}

func (s *Service) anonymize(ctx context.Context, res *Result, cutoff time.Time) error {
	for {
		var n int
		err := db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
			ids, err := q.AnonymizeVisitBatch(ctx, db.AnonymizeVisitBatchParams{Cutoff: cutoff, BatchSize: batchSize})
			n = len(ids)
			return err
		})
		if err != nil {
			return err
		}
		res.RecordsAnonymized += n
		if n == 0 {
			return nil
		}
	}
}

// finish records the outcome in retention_runs and the audit log. It uses a
// context that survives cancellation so an interrupted run still leaves a record.
func (s *Service) finish(ctx context.Context, trigger Trigger, actor *uuid.UUID, res Result, runErr error) {
	ctx = context.WithoutCancel(ctx)
	q := db.New(s.pool)
	var errText pgtype.Text
	switch {
	case runErr != nil:
		errText = pgtype.Text{String: runErr.Error(), Valid: true}
	case res.FilesFailed > 0:
		errText = pgtype.Text{String: fmt.Sprintf("%d photo file(s) could not be deleted; will retry on the next run", res.FilesFailed), Valid: true}
	}
	if err := q.FinishRetentionRun(ctx, db.FinishRetentionRunParams{
		ID: res.RunID, PhotosDeleted: int32(res.PhotosDeleted), RecordsAnonymized: int32(res.RecordsAnonymized), Error: errText,
	}); err != nil {
		slog.Error("retention: record run result", "run_id", res.RunID, "err", err)
	}
	user := pgtype.UUID{}
	if actor != nil {
		user = pgtype.UUID{Bytes: *actor, Valid: true}
	}
	if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: user, Action: "retention_run",
		Target: fmt.Sprintf("run:%d trigger:%s dry_run:%t photos:%d records:%d failed:%d",
			res.RunID, trigger, res.DryRun, res.PhotosDeleted, res.RecordsAnonymized, res.FilesFailed),
	}); err != nil {
		slog.Error("retention: audit run", "run_id", res.RunID, "err", err)
	}
}
