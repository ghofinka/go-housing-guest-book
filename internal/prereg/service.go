// Package prereg lets a homeowner announce an expected guest (PRD S2). The owner gets a short code;
// the guard looks it up or enters it at check-in (see internal/visit), after which it cannot be reused.
package prereg

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/visit"
)

var (
	ErrNotFound     = errors.New("pre-registration not found")
	ErrLimitReached = errors.New("too many active pre-registrations for this house")
	ErrAlreadyUsed  = errors.New("pre-registration already used")
	ErrNoHouse      = errors.New("owner account has no house")
	errCodeTaken    = errors.New("could not generate a unique code")
)

const (
	// MaxActivePerHouse caps pre-registrations that can still be used, so one account cannot
	// flood the table with codes.
	MaxActivePerHouse = 20
	// MaxDaysAhead is how far in advance a guest can be announced.
	MaxDaysAhead = 30

	statusActive    = "active"
	statusCancelled = "cancelled"
	statusUsed      = "used"

	codeAttempts = 5
)

type HouseRef struct {
	ID     uuid.UUID `json:"id"`
	Block  string    `json:"block"`
	Number string    `json:"number"`
}

type PreRegistration struct {
	ID           uuid.UUID `json:"id"`
	House        HouseRef  `json:"house"`
	GuestName    *string   `json:"guest_name"`
	ExpectedDate string    `json:"expected_date"` // YYYY-MM-DD, a day in WIB
	Plate        *string   `json:"plate"`
	Code         string    `json:"code"`
	Status       string    `json:"status"` // active, used or cancelled
	// Valid is true when the code can be used right now: active and today is the expected date.
	Valid      bool       `json:"valid"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UsedAt     *time.Time `json:"used_at"`
	Anonymized bool       `json:"anonymized"`
}

type CreateInput struct {
	GuestName string
	// ExpectedDate is the guest's day as midnight WIB (httpx.ParseDay).
	ExpectedDate time.Time
	Plate        *string
}

type ListFilter struct {
	Status  string // "", "active", "used" or "cancelled"
	HouseID *uuid.UUID
	Cursor  string
	Limit   int
}

type ListResult struct {
	Data       []PreRegistration `json:"data"`
	NextCursor *string           `json:"next_cursor"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, now: time.Now}
}

// Create records an expected guest for the owner's house and returns it with its code.
func (s *Service) Create(ctx context.Context, p auth.Principal, in CreateInput) (PreRegistration, error) {
	if p.HouseID == nil {
		return PreRegistration{}, ErrNoHouse
	}
	now := s.now()
	var out PreRegistration
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		active, err := q.CountActivePreRegistrations(ctx, db.CountActivePreRegistrationsParams{
			HouseID: *p.HouseID, ExpectedDate: calendarDate(now.In(httpx.WIB)),
		})
		if err != nil {
			return fmt.Errorf("count active: %w", err)
		}
		if active >= MaxActivePerHouse {
			return ErrLimitReached
		}
		day := in.ExpectedDate.In(httpx.WIB)
		params := db.CreatePreRegistrationParams{
			HouseID:      *p.HouseID,
			GuestName:    pgtype.Text{String: in.GuestName, Valid: true},
			ExpectedDate: calendarDate(day),
			Plate:        textOrNull(in.Plate),
			// A code works only on its day: it expires at the end of that day in WIB.
			ExpiresAt: time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, httpx.WIB),
			CreatedBy: p.UserID,
		}
		var id uuid.UUID
		for attempt := 0; ; attempt++ {
			if attempt == codeAttempts {
				return errCodeTaken
			}
			if params.Code, err = newCode(); err != nil {
				return err
			}
			id, err = q.CreatePreRegistration(ctx, params)
			if errors.Is(err, pgx.ErrNoRows) { // code already taken, try another
				continue
			}
			if err != nil {
				return fmt.Errorf("create pre-registration: %w", err)
			}
			break
		}
		r, err := q.GetPreRegistration(ctx, id)
		if err != nil {
			return fmt.Errorf("reload pre-registration: %w", err)
		}
		out = toPreRegistration(r, now)
		return audit(ctx, q, p.UserID, "prereg_created", id)
	})
	if err != nil {
		return PreRegistration{}, wrap(err, "create")
	}
	return out, nil
}

// Get returns one pre-registration. Another house's id looks exactly like a missing one.
func (s *Service) Get(ctx context.Context, p auth.Principal, id uuid.UUID) (PreRegistration, error) {
	now := s.now()
	var out PreRegistration
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		r, err := q.GetPreRegistration(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !visibleTo(p, r.HouseID)) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get pre-registration: %w", err)
		}
		out = toPreRegistration(r, now)
		return audit(ctx, q, p.UserID, "prereg_viewed", id)
	})
	if err != nil {
		return PreRegistration{}, wrap(err, "get")
	}
	return out, nil
}

// LookupByCode is the guard's lookup. Unknown codes are a 404; used, cancelled and wrong-day
// codes are returned with their status so the guard can tell the guest why it fails.
func (s *Service) LookupByCode(ctx context.Context, p auth.Principal, code string) (PreRegistration, error) {
	now := s.now()
	var out PreRegistration
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		r, err := q.GetPreRegistrationByCode(ctx, code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lookup pre-registration: %w", err)
		}
		out = toPreRegistration(row(r), now)
		return audit(ctx, q, p.UserID, "prereg_viewed", r.ID)
	})
	if err != nil {
		return PreRegistration{}, wrap(err, "lookup")
	}
	return out, nil
}

// Cancel withdraws an active pre-registration and blanks its guest name and plate.
// It is idempotent; a code that was already used cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, p auth.Principal, id uuid.UUID) (PreRegistration, error) {
	now := s.now()
	var out PreRegistration
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		r, err := q.GetPreRegistration(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !visibleTo(p, r.HouseID)) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get pre-registration: %w", err)
		}
		if r.Status == statusUsed {
			return ErrAlreadyUsed
		}
		if r.Status == statusActive {
			if _, err := q.CancelPreRegistration(ctx, id); err != nil {
				return fmt.Errorf("cancel pre-registration: %w", err)
			}
			if err := audit(ctx, q, p.UserID, "prereg_cancelled", id); err != nil {
				return err
			}
			if r, err = q.GetPreRegistration(ctx, id); err != nil {
				return fmt.Errorf("reload pre-registration: %w", err)
			}
		}
		out = toPreRegistration(r, now)
		return nil
	})
	if err != nil {
		return PreRegistration{}, wrap(err, "cancel")
	}
	return out, nil
}

// List returns pre-registrations newest first. An owner only ever sees their own house.
func (s *Service) List(ctx context.Context, p auth.Principal, f ListFilter) (ListResult, error) {
	if p.Role == auth.RoleOwner {
		if p.HouseID == nil || (f.HouseID != nil && *f.HouseID != *p.HouseID) {
			return ListResult{Data: []PreRegistration{}}, nil
		}
		f.HouseID = p.HouseID
	}
	var afterAt pgtype.Timestamptz
	var afterID pgtype.UUID
	if f.Cursor != "" {
		at, id, err := httpx.DecodeCursor(f.Cursor)
		if err != nil {
			return ListResult{}, httpx.ErrBadCursor
		}
		afterAt = pgtype.Timestamptz{Time: at, Valid: true}
		afterID = pgtype.UUID{Bytes: id, Valid: true}
	}
	now := s.now()
	var rows []row
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		target := "houses:all"
		if f.HouseID != nil {
			target = "house:" + f.HouseID.String()
		}
		if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: p.UserID, Valid: true}, Action: "preregs_listed", Target: target,
		}); err != nil {
			return err
		}
		rs, err := q.ListPreRegistrations(ctx, db.ListPreRegistrationsParams{
			Status: textOrNullStr(f.Status), HouseID: uuidOrNull(f.HouseID),
			AfterCreatedAt: afterAt, AfterID: afterID, PageSize: int32(f.Limit) + 1,
		})
		for _, r := range rs {
			rows = append(rows, row(r))
		}
		return err
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("list pre-registrations: %w", err)
	}
	res := ListResult{Data: []PreRegistration{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, toPreRegistration(r, now))
	}
	return res, nil
}

// row is the shared shape of every pre-registration query result (identical columns).
type row = db.GetPreRegistrationRow

func toPreRegistration(r row, now time.Time) PreRegistration {
	out := PreRegistration{
		ID:           r.ID,
		House:        HouseRef{ID: r.HouseID, Block: r.Block, Number: r.Number},
		GuestName:    textPtr(r.GuestName),
		ExpectedDate: r.ExpectedDate.Time.Format(time.DateOnly),
		Plate:        textPtr(r.Plate),
		Code:         r.Code,
		Status:       r.Status,
		Valid:        r.Status == statusActive && visit.UsableOn(r.ExpectedDate.Time, now),
		ExpiresAt:    r.ExpiresAt.In(httpx.WIB),
		CreatedAt:    r.CreatedAt.In(httpx.WIB),
		Anonymized:   r.Anonymized,
	}
	if r.UsedAt.Valid {
		t := r.UsedAt.Time.In(httpx.WIB)
		out.UsedAt = &t
	}
	return out
}

// visibleTo is the service-level house check, a second layer on top of row-level security.
func visibleTo(p auth.Principal, houseID uuid.UUID) bool {
	if p.Role != auth.RoleOwner {
		return true
	}
	return p.HouseID != nil && *p.HouseID == houseID
}

// newCode returns a random code from visit.CodeAlphabet. The alphabet has 32 characters, which
// divides 256 evenly, so taking each random byte modulo 32 introduces no bias.
func newCode() (string, error) {
	var b [visit.CodeLength]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random code: %w", err)
	}
	code := make([]byte, visit.CodeLength)
	for i, v := range b {
		code[i] = visit.CodeAlphabet[int(v)%len(visit.CodeAlphabet)]
	}
	return string(code), nil
}

// calendarDate keeps only the Y-M-D of t, as the date column expects (no time zone shift).
func calendarDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

func audit(ctx context.Context, q *db.Queries, actor uuid.UUID, action string, id uuid.UUID) error {
	// The target is the id only: never the guest name or plate.
	return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: action, Target: "prereg:" + id.String(),
	})
}

func wrap(err error, op string) error {
	for _, known := range []error{ErrNotFound, ErrLimitReached, ErrAlreadyUsed, ErrNoHouse} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%s pre-registration: %w", op, err)
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

func textOrNullStr(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func uuidOrNull(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}
