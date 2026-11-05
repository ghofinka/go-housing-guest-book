// Package visit records guests entering and leaving the cluster (M1-M3).
package visit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	ErrVisitClosed       = errors.New("visit already checked out")
	ErrDecisionMade      = errors.New("owner decision already made")
	ErrBadCursor         = httpx.ErrBadCursor

	// Device-recorded times (PRD S5) are only plausible within a window around the server's clock.
	ErrClientTimeFuture = errors.New("client captured time is in the future")
	ErrClientTimeOld    = errors.New("client captured time is too old")
)

// Offline sync window (PRD S5): a device may record an event up to MaxOfflineAge ago (a guard post
// can be offline over a weekend) and its clock may run ClockSkew ahead of the server's.
const (
	MaxOfflineAge = 72 * time.Hour
	ClockSkew     = 5 * time.Minute
)

// checkClientTime validates a device-recorded time against the server clock.
func checkClientTime(t *time.Time, now time.Time) error {
	switch {
	case t == nil:
		return nil
	case t.After(now.Add(ClockSkew)):
		return ErrClientTimeFuture
	case t.Before(now.Add(-MaxOfflineAge)):
		return ErrClientTimeOld
	}
	return nil
}

// clientTime renders a stored device time in WIB, or nil when there is none.
func clientTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	w := t.Time.In(httpx.WIB)
	return &w
}

func timestamptzOrNullPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t.UTC(), Valid: true}
}

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
	// OwnerDecision is the house owner's answer (PRD S4). It is advisory: the guard decides at the gate.
	OwnerDecision OwnerDecision `json:"owner_decision"`
	// Overstay is true while the guest is still inside after the configured limit (PRD S7, setting
	// long_stay_hours, default 12), counted from the server's check-in time.
	Overstay bool `json:"overstay"`
	// ClientCapturedAt and ClientCheckoutCapturedAt are the times the guard's device recorded the
	// check-in and check-out when it was offline (PRD S5). They sit beside checkin_at and checkout_at,
	// which stay the server's receive times; null when the request was made online.
	ClientCapturedAt         *time.Time `json:"client_captured_at"`
	ClientCheckoutCapturedAt *time.Time `json:"client_checkout_captured_at"`
}

// Owner decision statuses, as stored in visit_decisions plus the default before an answer.
const (
	DecisionPending  = "pending"
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

// OwnerDecision reports whether the owner has answered. A visit made from a pre-registration is
// approved with source "pre_registration" until the owner says otherwise; Source is null while pending.
type OwnerDecision struct {
	Status    string     `json:"status"`
	DecidedAt *time.Time `json:"decided_at"`
	Source    *string    `json:"source"`
}

// ValidDecision reports whether s is an answer an owner may send.
func ValidDecision(s string) bool { return s == DecisionApproved || s == DecisionRejected }

// Decide records the owner's answer for one visit. The first answer stands: sending the same one
// again is a no-op, a different one is refused. It is only allowed while the guest is inside.
func (s *Service) Decide(ctx context.Context, p auth.Principal, id uuid.UUID, decision string) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		r, err := q.GetVisit(ctx, id)
		// Another house's visit looks exactly like a missing one (RLS hides it; the check is a second layer).
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (p.HouseID == nil || r.HouseID != *p.HouseID)) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get visit: %w", err)
		}

		existing, err := q.GetVisitDecision(ctx, id)
		switch {
		case err == nil:
			if existing != decision {
				return ErrDecisionMade
			}
			out = toVisit(row(r)) // same answer again: nothing to write
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("get decision: %w", err)
		}
		if r.Status != StatusInside {
			return ErrVisitClosed
		}

		_, err = q.InsertVisitDecision(ctx, db.InsertVisitDecisionParams{
			VisitID: id, HouseID: r.HouseID, Decision: decision, DecidedBy: p.UserID,
		})
		if errors.Is(err, pgx.ErrNoRows) { // answered concurrently
			if existing, err = q.GetVisitDecision(ctx, id); err != nil {
				return fmt.Errorf("get decision: %w", err)
			}
			if existing != decision {
				return ErrDecisionMade
			}
		} else if err != nil {
			return fmt.Errorf("insert decision: %w", err)
		} else if err := audit(ctx, q, p.UserID, "visit_decided", id); err != nil {
			return err
		}
		reloaded, err := q.GetVisit(ctx, id)
		if err != nil {
			return fmt.Errorf("reload visit: %w", err)
		}
		out = toVisit(row(reloaded))
		return nil
	})
	if err != nil {
		return Visit{}, wrap(err, "decide")
	}
	return forCaller(p, out), nil
}

type CheckInInput struct {
	// ClientRequestID makes a retry (flaky signal, double tap) return the original visit.
	ClientRequestID *uuid.UUID
	HouseID         uuid.UUID
	GuestName       *string
	Category        string
	PurposeNote     *string
	VehiclePlate    *string
	// PreRegistrationCode is a normalized pre-registration code (PRD S2), "" for none. When set,
	// the house, name and plate default from the pre-registration.
	PreRegistrationCode string
	// ClientCapturedAt is when the device recorded the check-in while offline (PRD S5), nil online.
	// It is stored beside the server's check-in time, never instead of it.
	ClientCapturedAt *time.Time
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
	// Search filters (PRD S6). Empty means no filter. Name is a case-insensitive contains match,
	// Plate a contains match on the plate without spaces (already upper-cased by the handler).
	Category, Name, Plate string
	// Decision filters by the owner's answer: pending, approved or rejected (PRD S4).
	Decision string
	// OverstayOnly keeps only guests still inside after the long-stay limit (PRD S7).
	OverstayOnly bool
}

// likeContains turns user text into a contains pattern for ILIKE, escaping the LIKE
// metacharacters so the text is always matched literally.
func likeContains(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
	return pgtype.Text{String: "%" + esc + "%", Valid: true}
}

func textOrNullStr(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
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

		// Checked after the replay on purpose: a retry of a check-in that already synced must
		// still answer 200 even if the device clock or the 72-hour window has moved on since.
		if err := checkClientTime(in.ClientCapturedAt, time.Now()); err != nil {
			return err
		}

		var preRegID pgtype.UUID
		if in.PreRegistrationCode != "" {
			var err error
			preRegID, err = useCode(ctx, q, &in, time.Now())
			if errors.Is(err, ErrPreRegUsed) {
				// A concurrent retry of this same check-in may have just used the code:
				// answer like a replay instead of reporting the code as used.
				if v, ok, rerr := replay(ctx, q, in.ClientRequestID); rerr == nil && ok {
					out = v
					return nil
				}
			}
			if err != nil {
				return err
			}
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

		// The plate may come from the pre-registration, so this is checked after resolving the code.
		if in.VehiclePlate != nil && in.VehiclePhoto == nil && !FastLane(in.Category) {
			return ErrVehiclePhotoRequired
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
			ClientRequestID:   uuidOrNull(in.ClientRequestID),
			HouseID:           in.HouseID,
			GuestName:         textOrNull(in.GuestName),
			Category:          in.Category,
			PurposeNote:       textOrNull(in.PurposeNote),
			VehiclePlate:      textOrNull(in.VehiclePlate),
			CheckinBy:         p.UserID,
			VehiclePhotoPath:  textOrNull(vehicleKey),
			FacePhotoPath:     textOrNull(faceKey),
			PreRegistrationID: preRegID,
			ClientCapturedAt:  timestamptzOrNullPtr(in.ClientCapturedAt),
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
		if preRegID.Valid {
			if _, err := q.MarkPreRegistrationUsed(ctx, preRegID.Bytes); err != nil {
				return fmt.Errorf("mark pre-registration used: %w", err)
			}
			if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
				UserID: pgtype.UUID{Bytes: p.UserID, Valid: true}, Action: "prereg_used",
				Target: "prereg:" + uuid.UUID(preRegID.Bytes).String(),
			}); err != nil {
				return err
			}
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
//
// clientCapturedAt is the time the guard's device recorded the exit while offline (PRD S5), nil
// online. It is validated and stored only when this call is the one that checks the guest out.
func (s *Service) CheckOut(ctx context.Context, p auth.Principal, id uuid.UUID, clientCapturedAt *time.Time) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		var err error
		out, err = checkOut(ctx, q, p, id, clientCapturedAt)
		return err
	})
	if err != nil {
		return Visit{}, wrap(err, "check out")
	}
	return out, nil
}

// CheckOutByRequest is CheckOut for a client that only knows the client_request_id it used at
// check-in, which is all an offline device has for a guest it registered without a connection.
// An unknown key is ErrNotFound: the check-in has to be replayed first.
func (s *Service) CheckOutByRequest(ctx context.Context, p auth.Principal, requestID uuid.UUID, clientCapturedAt *time.Time) (Visit, error) {
	var out Visit
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		r, err := q.GetVisitByClientRequestID(ctx, pgtype.UUID{Bytes: requestID, Valid: true})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lookup client request: %w", err)
		}
		out, err = checkOut(ctx, q, p, r.ID, clientCapturedAt)
		return err
	})
	if err != nil {
		return Visit{}, wrap(err, "check out by request")
	}
	return out, nil
}

// checkOut does the work of a check-out inside the caller's transaction. Only the first call changes
// the row and is audited; a repeat returns the visit as it was, whatever device time it carries.
func checkOut(ctx context.Context, q *db.Queries, p auth.Principal, id uuid.UUID, clientCapturedAt *time.Time) (Visit, error) {
	cur, err := q.GetVisit(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Visit{}, ErrNotFound
	}
	if err != nil {
		return Visit{}, fmt.Errorf("get visit: %w", err)
	}
	if cur.Status == StatusExited {
		return toVisit(cur), nil
	}
	if err := checkClientTime(clientCapturedAt, time.Now()); err != nil {
		return Visit{}, err
	}

	_, err = q.CheckoutVisit(ctx, db.CheckoutVisitParams{
		ID:                       id,
		CheckoutBy:               pgtype.UUID{Bytes: p.UserID, Valid: true},
		ClientCheckoutCapturedAt: timestamptzOrNullPtr(clientCapturedAt),
	})
	changed := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) { // ErrNoRows: another request exited it first
		return Visit{}, fmt.Errorf("checkout: %w", err)
	}
	reloaded, err := q.GetVisit(ctx, id)
	if err != nil {
		return Visit{}, fmt.Errorf("reload visit: %w", err)
	}
	if changed {
		if err := audit(ctx, q, p.UserID, "visit_checked_out", id); err != nil {
			return Visit{}, err
		}
	}
	return toVisit(reloaded), nil
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
				FromAt: timestamptzOrNull(f.From), BeforeAt: timestamptzOrNull(f.Before),
				Category: textOrNullStr(f.Category), NameLike: likeContains(f.Name), PlateLike: likeContains(f.Plate), Decision: textOrNullStr(f.Decision),
				OverstayOnly: pgtype.Bool{Bool: f.OverstayOnly, Valid: f.OverstayOnly}, PageSize: size})
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
			FromAt: timestamptzOrNull(f.From), BeforeAt: timestamptzOrNull(f.Before),
			Category: textOrNullStr(f.Category), NameLike: likeContains(f.Name), PlateLike: likeContains(f.Plate), Decision: textOrNullStr(f.Decision),
			OverstayOnly: pgtype.Bool{Bool: f.OverstayOnly, Valid: f.OverstayOnly}, PageSize: size})
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
	for _, known := range []error{
		ErrNotFound, ErrPhotoNotFound, ErrHouseInvalid, ErrFacePhotoRequired, ErrVehiclePhotoRequired,
		ErrPreRegNotFound, ErrPreRegUsed, ErrPreRegCancelled, ErrPreRegWrongDate, ErrPreRegHouseMismatch,
		ErrVisitClosed, ErrDecisionMade, ErrClientTimeFuture, ErrClientTimeOld,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

func toVisit(r row) Visit {
	v := Visit{
		ID:                       r.ID,
		House:                    HouseRef{ID: r.HouseID, Block: r.Block, Number: r.Number},
		GuestName:                textPtr(r.GuestName),
		Category:                 r.Category,
		PurposeNote:              textPtr(r.PurposeNote),
		VehiclePlate:             textPtr(r.VehiclePlate),
		CheckinAt:                r.CheckinAt.In(httpx.WIB),
		CheckinBy:                &r.CheckinBy,
		Status:                   r.Status,
		Anonymized:               r.Anonymized,
		Overstay:                 r.Overstay,
		ClientCapturedAt:         clientTime(r.ClientCapturedAt),
		ClientCheckoutCapturedAt: clientTime(r.ClientCheckoutCapturedAt),
		OwnerDecision:            OwnerDecision{Status: r.OwnerDecision},
	}
	if r.OwnerDecisionSource != "" {
		src := r.OwnerDecisionSource
		v.OwnerDecision.Source = &src
	}
	if r.OwnerDecidedAt.Valid {
		t := r.OwnerDecidedAt.Time.In(httpx.WIB)
		v.OwnerDecision.DecidedAt = &t
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
