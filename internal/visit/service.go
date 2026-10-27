// Package visit records guests entering and leaving the cluster (M1-M3).
package visit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/photo"
)

var (
	ErrNotFound          = errors.New("visit not found")
	ErrPhotoNotFound     = errors.New("photo not found")
	ErrHouseInvalid      = errors.New("house missing or inactive")
	ErrFacePhotoRequired = errors.New("face photo required by cluster policy")
	ErrBadCursor         = httpx.ErrBadCursor
)

// Photo kinds, as used in the URL and in audit targets.
const (
	KindVehicle = "vehicle"
	KindFace    = "face"
)

// settingFacePhotoRequired is the settings key for the cluster's face-photo policy.
const settingFacePhotoRequired = "face_photo_required"

// FastLane reports whether a category uses the courier/ojol fast lane: no guest
// name and no face photo needed (PRD S1 and the MVP form appendix).
func FastLane(category string) bool { return category == "kurir" || category == "ojol" }

const (
	StatusInside = "inside"
	StatusExited = "exited"
)

type HouseRef struct {
	ID     uuid.UUID `json:"id"`
	Block  string    `json:"block"`
	Number string    `json:"number"`
}

// Visit is the API view of a visit. Times are rendered in WIB.
type Visit struct {
	ID           uuid.UUID `json:"id"`
	House        HouseRef  `json:"house"`
	GuestName    *string   `json:"guest_name"`
	Category     string    `json:"category"`
	PurposeNote  *string   `json:"purpose_note"`
	VehiclePlate *string   `json:"vehicle_plate"`
	CheckinAt    time.Time `json:"checkin_at"`
	// CheckinBy and CheckoutBy identify the guard on duty. Owners do not get them.
	CheckinBy  *uuid.UUID `json:"checkin_by"`
	CheckoutAt *time.Time `json:"checkout_at"`
	CheckoutBy *uuid.UUID `json:"checkout_by"`
	Status     string     `json:"status"`
	// Anonymized is true once retention or an erasure request has blanked the personal fields.
	Anonymized bool `json:"anonymized"`
	// Photo URLs are API paths that need the same Bearer token; they are not public links.
	VehiclePhotoURL *string `json:"vehicle_photo_url"`
	FacePhotoURL    *string `json:"face_photo_url"`
}

type CheckInInput struct {
	// ClientRequestID makes a retry (flaky signal, double tap) return the original visit.
	ClientRequestID *uuid.UUID
	HouseID         uuid.UUID
	GuestName       *string
	Category        string
	PurposeNote     *string
	VehiclePlate    *string
	// Photos are already validated and compressed (photo.Process). nil means none.
	VehiclePhoto []byte
	FacePhoto    []byte
}

type ListFilter struct {
	Status  string // "", "inside" or "exited"
	HouseID *uuid.UUID
	// From (inclusive) and Before (exclusive) bound the check-in time.
	From, Before *time.Time
	Cursor       string
	Limit        int
}

type ListResult struct {
	Data       []Visit `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type Service struct {
	pool   *pgxpool.Pool
	photos photo.Store
}

func NewService(pool *pgxpool.Pool, photos photo.Store) *Service {
	return &Service{pool: pool, photos: photos}
}

// CheckIn records a guest entering. The entry time is the database clock, never client input.
// created is false when client_request_id matched an earlier check-in.
//
// Photos are written to the store only after every check has passed, and are
// deleted again if the transaction does not end up creating the visit (rollback,
// or a retry that matched an earlier check-in), so no orphan files are left behind.
func (s *Service) CheckIn(ctx context.Context, p auth.Principal, in CheckInInput) (out Visit, created bool, err error) {
	var saved []string
	defer func() {
		if err == nil && created {
			return
		}
		for _, key := range saved {
			if delErr := s.photos.Delete(context.WithoutCancel(ctx), key); delErr != nil {
				slog.Error("cleanup orphan photo", "err", delErr)
			}
		}
	}()

	err = db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		// A retry must succeed even if the house was deactivated meanwhile.
		if v, ok, err := replay(ctx, q, in.ClientRequestID); err != nil || ok {
			out = v
			return err
		}

		h, err := q.GetHouse(ctx, in.HouseID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !h.Active) {
			return ErrHouseInvalid
		}
		if err != nil {
			return fmt.Errorf("get house: %w", err)
		}

		if in.FacePhoto == nil && !FastLane(in.Category) {
			required, err := q.GetSetting(ctx, settingFacePhotoRequired)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("get setting: %w", err)
			}
			if required == "true" {
				return ErrFacePhotoRequired
			}
		}

		var vehicleKey, faceKey *string
		for _, ph := range []struct {
			data []byte
			key  **string
		}{{in.VehiclePhoto, &vehicleKey}, {in.FacePhoto, &faceKey}} {
			if ph.data == nil {
				continue
			}
			key, err := s.photos.Save(ctx, ph.data)
			if err != nil {
				return fmt.Errorf("save photo: %w", err)
			}
			saved = append(saved, key)
			*ph.key = &key
		}

		id, err := q.CreateVisit(ctx, db.CreateVisitParams{
			ClientRequestID:  uuidOrNull(in.ClientRequestID),
			HouseID:          in.HouseID,
			GuestName:        textOrNull(in.GuestName),
			Category:         in.Category,
			PurposeNote:      textOrNull(in.PurposeNote),
			VehiclePlate:     textOrNull(in.VehiclePlate),
			CheckinBy:        p.UserID,
			VehiclePhotoPath: textOrNull(vehicleKey),
			FacePhotoPath:    textOrNull(faceKey),
		})
		if errors.Is(err, pgx.ErrNoRows) { // lost a race with the same client_request_id
			v, ok, err := replay(ctx, q, in.ClientRequestID)
			if err != nil || !ok {
				return fmt.Errorf("replay after conflict: %w", errors.Join(err, pgx.ErrNoRows))
			}
			out = v
			return nil
		}
		if err != nil {
			return fmt.Errorf("create visit: %w", err)
		}
		row, err := q.GetVisit(ctx, id)
		if err != nil {
			return fmt.Errorf("reload visit: %w", err)
		}
		out, created = toVisit(row), true
		return audit(ctx, q, p.UserID, "visit_checked_in", id)
	})
	if err != nil {
		return Visit{}, false, wrap(err, "check in")
	}
	return out, created, nil
}

// CheckOut marks a guest as exited. It is idempotent: an already-exited visit is
// returned unchanged, keeping its original exit time.
func (s *Service) CheckOut(ctx context.Context, p auth.Principal, id uuid.UUID) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		_, err := q.CheckoutVisit(ctx, db.CheckoutVisitParams{ID: id, CheckoutBy: pgtype.UUID{Bytes: p.UserID, Valid: true}})
		changed := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("checkout: %w", err)
		}
		row, err := q.GetVisit(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("reload visit: %w", err)
		}
		out = toVisit(row)
		if changed {
			return audit(ctx, q, p.UserID, "visit_checked_out", id)
		}
		return nil
	})
	if err != nil {
		return Visit{}, wrap(err, "check out")
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, p auth.Principal, id uuid.UUID) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		row, err := q.GetVisit(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out = forCaller(p, toVisit(row))
		// Only an existing visit the caller may see gets here, so refused or missing lookups are not logged as views.
		return audit(ctx, q, p.UserID, "visit_viewed", id)
	})
	if err != nil {
		return Visit{}, wrap(err, "get visit")
	}
	return out, nil
}

// Erase deletes a visit's photos and blanks its personal fields right now, for a
// data-subject request. The row stays for statistics. It is idempotent: a visit that
// is already erased (by this call or by retention) is returned as is, with no second
// audit row. If a photo file cannot be deleted nothing is cleared and the error is
// returned, so the request can simply be repeated.
func (s *Service) Erase(ctx context.Context, p auth.Principal, id uuid.UUID) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		st, err := q.GetVisitErasureState(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get erasure state: %w", err)
		}
		if !(st.AnonymizedAt.Valid && !st.VehiclePhotoPath.Valid && !st.FacePhotoPath.Valid) {
			for _, path := range []pgtype.Text{st.VehiclePhotoPath, st.FacePhotoPath} {
				if !path.Valid {
					continue
				}
				if err := s.photos.Delete(ctx, path.String); err != nil {
					return fmt.Errorf("delete photo file: %w", err)
				}
			}
			if err := q.EraseVisit(ctx, id); err != nil {
				return fmt.Errorf("erase visit: %w", err)
			}
			if err := audit(ctx, q, p.UserID, "visit_erased", id); err != nil {
				return err
			}
		}
		row, err := q.GetVisit(ctx, id)
		if err != nil {
			return fmt.Errorf("reload visit: %w", err)
		}
		out = forCaller(p, toVisit(row))
		return nil
	})
	if err != nil {
		return Visit{}, wrap(err, "erase visit")
	}
	return out, nil
}

// OpenPhoto returns a visit photo for download. The visit lookup runs under the
// caller's row-level-security context, so a caller who cannot see the visit gets
// ErrNotFound exactly as if it did not exist. Every successful view is audited.
func (s *Service) OpenPhoto(ctx context.Context, p auth.Principal, id uuid.UUID, kind string) (io.ReadSeekCloser, error) {
	var f io.ReadSeekCloser
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		paths, err := q.GetVisitPhotoPaths(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get photo path: %w", err)
		}
		path := paths.VehiclePhotoPath
		if kind == KindFace {
			path = paths.FacePhotoPath
		}
		if !path.Valid {
			return ErrPhotoNotFound
		}
		f, err = s.photos.Open(ctx, path.String)
		if errors.Is(err, photo.ErrNotFound) {
			return ErrPhotoNotFound // e.g. already purged by retention
		}
		if err != nil {
			return err
		}
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: p.UserID, Valid: true},
			Action: "photo_viewed", Target: "visit:" + id.String() + ":" + kind,
		})
	})
	if err != nil {
		if f != nil {
			f.Close()
		}
		return nil, wrap(err, "open photo")
	}
	return f, nil
}

func (s *Service) List(ctx context.Context, p auth.Principal, f ListFilter) (ListResult, error) {
	// An owner only ever lists their own house. Asking for another house is not an
	// error (that would confirm it exists); it simply matches nothing, like RLS.
	if p.Role == auth.RoleOwner {
		if f.HouseID != nil && (p.HouseID == nil || *f.HouseID != *p.HouseID) {
			return ListResult{Data: []Visit{}}, nil
		}
		f.HouseID = p.HouseID
	}

	var afterAt pgtype.Timestamptz
	var afterID pgtype.UUID
	if f.Cursor != "" {
		at, id, err := httpx.DecodeCursor(f.Cursor)
		if err != nil {
			return ListResult{}, ErrBadCursor
		}
		afterAt = pgtype.Timestamptz{Time: at, Valid: true}
		afterID = pgtype.UUID{Bytes: id, Valid: true}
	}
	size := int32(f.Limit) + 1 // one extra row tells us whether another page exists

	var rows []row
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		// Owner and admin history views are audited. Guards' lists are not: the
		// "guests inside" screen refreshes constantly and would bury everything else.
		if p.Role != auth.RoleGuard {
			target := "houses:all"
			if f.HouseID != nil {
				target = "house:" + f.HouseID.String()
			}
			if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
				UserID: pgtype.UUID{Bytes: p.UserID, Valid: true}, Action: "visits_listed", Target: target,
			}); err != nil {
				return err
			}
		}
		if f.Status == StatusInside {
			rs, err := q.ListInsideVisits(ctx, db.ListInsideVisitsParams{
				HouseID: uuidOrNull(f.HouseID), AfterCheckinAt: afterAt, AfterID: afterID,
				FromAt: timestamptzOrNull(f.From), BeforeAt: timestamptzOrNull(f.Before), PageSize: size})
			for _, r := range rs {
				rows = append(rows, row(r))
			}
			return err
		}
		status := pgtype.Text{}
		if f.Status != "" {
			status = pgtype.Text{String: f.Status, Valid: true}
		}
		rs, err := q.ListVisits(ctx, db.ListVisitsParams{
			Status: status, HouseID: uuidOrNull(f.HouseID), AfterCheckinAt: afterAt, AfterID: afterID,
			FromAt: timestamptzOrNull(f.From), BeforeAt: timestamptzOrNull(f.Before), PageSize: size})
		for _, r := range rs {
			rows = append(rows, row(r))
		}
		return err
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("list visits: %w", err)
	}

	res := ListResult{Data: []Visit{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CheckinAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, forCaller(p, toVisit(r)))
	}
	return res, nil
}

// row is the shared shape of every visit query result (they select identical columns).
type row = db.GetVisitRow

func replay(ctx context.Context, q *db.Queries, id *uuid.UUID) (Visit, bool, error) {
	if id == nil {
		return Visit{}, false, nil
	}
	r, err := q.GetVisitByClientRequestID(ctx, pgtype.UUID{Bytes: *id, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return Visit{}, false, nil
	}
	if err != nil {
		return Visit{}, false, fmt.Errorf("lookup client request: %w", err)
	}
	return toVisit(row(r)), true, nil
}

func audit(ctx context.Context, q *db.Queries, actor uuid.UUID, action string, visitID uuid.UUID) error {
	return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: action, Target: "visit:" + visitID.String(),
	})
}

func wrap(err error, op string) error {
	for _, known := range []error{ErrNotFound, ErrPhotoNotFound, ErrHouseInvalid, ErrFacePhotoRequired} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func toVisit(r row) Visit {
	v := Visit{
		ID:           r.ID,
		House:        HouseRef{ID: r.HouseID, Block: r.Block, Number: r.Number},
		GuestName:    textPtr(r.GuestName),
		Category:     r.Category,
		PurposeNote:  textPtr(r.PurposeNote),
		VehiclePlate: textPtr(r.VehiclePlate),
		CheckinAt:    r.CheckinAt.In(httpx.WIB),
		CheckinBy:    &r.CheckinBy,
		Status:       r.Status,
		Anonymized:   r.Anonymized,
	}
	if r.HasVehiclePhoto {
		u := "/v1/visits/" + r.ID.String() + "/photos/" + KindVehicle
		v.VehiclePhotoURL = &u
	}
	if r.HasFacePhoto {
		u := "/v1/visits/" + r.ID.String() + "/photos/" + KindFace
		v.FacePhotoURL = &u
	}
	if r.CheckoutAt.Valid {
		t := r.CheckoutAt.Time.In(httpx.WIB)
		v.CheckoutAt = &t
	}
	if r.CheckoutBy.Valid {
		id := uuid.UUID(r.CheckoutBy.Bytes)
		v.CheckoutBy = &id
	}
	return v
}

// forCaller hides what the caller's role has no need to know: owners see their
// guests and times, not which guard handled them.
func forCaller(p auth.Principal, v Visit) Visit {
	if p.Role == auth.RoleOwner {
		v.CheckinBy, v.CheckoutBy = nil, nil
	}
	return v
}

func timestamptzOrNull(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	return &t.String
}

func textOrNull(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func uuidOrNull(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}
