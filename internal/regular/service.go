// Package regular lets a homeowner keep a list of regular guests (a maid, a driver, a tutor) they have
// approved once (PRD C2). The guard then checks one in by picking the entry (see internal/visit), and the
// owner's approval counts as given. The list is the owner's standing consent: it does not expire by itself
// and the owner deletes an entry whenever they like.
package regular

import (
	"context"
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
)

var (
	ErrNotFound     = errors.New("regular guest not found")
	ErrLimitReached = errors.New("too many active regular guests for this house")
	ErrNoHouse      = errors.New("owner account has no house")
)

// MaxActivePerHouse caps the active entries of one house, so one account cannot fill the table.
const MaxActivePerHouse = 20

type HouseRef struct {
	ID     uuid.UUID `json:"id"`
	Block  string    `json:"block"`
	Number string    `json:"number"`
}

// Guest is one regular guest as the API shows it.
type Guest struct {
	ID        uuid.UUID `json:"id"`
	House     HouseRef  `json:"house"`
	GuestName string    `json:"guest_name"`
	Plate     *string   `json:"plate"`
	Category  string    `json:"category"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateInput struct {
	GuestName string
	Plate     *string // already normalized
	Category  string
}

// UpdateInput changes the fields that are set; a nil field stays as it is.
type UpdateInput struct {
	GuestName  *string
	Plate      *string // already normalized
	ClearPlate bool
	Category   *string
	Active     *bool
}

type ListFilter struct {
	HouseID *uuid.UUID
	Active  *bool
	Cursor  string
	Limit   int
}

type ListResult struct {
	Data       []Guest `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create adds a regular guest to the owner's own house.
func (s *Service) Create(ctx context.Context, p auth.Principal, in CreateInput) (Guest, error) {
	if p.HouseID == nil {
		return Guest{}, ErrNoHouse
	}
	var out Guest
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		if err := checkCap(ctx, q, *p.HouseID); err != nil {
			return err
		}
		id, err := q.CreateRegularGuest(ctx, db.CreateRegularGuestParams{
			HouseID: *p.HouseID, GuestName: in.GuestName, Plate: textOrNull(in.Plate), Category: in.Category, CreatedBy: p.UserID,
		})
		if err != nil {
			return fmt.Errorf("create regular guest: %w", err)
		}
		g, err := q.GetRegularGuest(ctx, id)
		if err != nil {
			return fmt.Errorf("reload regular guest: %w", err)
		}
		out = toGuest(row(g))
		return audit(ctx, q, p.UserID, "regular_guest_created", id)
	})
	if err != nil {
		return Guest{}, wrap(err, "create")
	}
	return out, nil
}

// List returns regular guests newest first. An owner only ever sees their own house; a guard or admin
// sees every house (filter with house_id).
func (s *Service) List(ctx context.Context, p auth.Principal, f ListFilter) (ListResult, error) {
	if p.Role == auth.RoleOwner {
		if p.HouseID == nil || (f.HouseID != nil && *f.HouseID != *p.HouseID) {
			// Asking for another house is not an error (that would confirm it exists): it matches nothing.
			return ListResult{Data: []Guest{}}, nil
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
	var rows []row
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		// An owner's or admin's look at the list is audited; a guard's is not, like the guard's visit list:
		// the gate screen refreshes constantly and would bury everything else.
		if p.Role != auth.RoleGuard {
			target := "houses:all"
			if f.HouseID != nil {
				target = "house:" + f.HouseID.String()
			}
			if err := q.InsertAuditLog(ctx, db.InsertAuditLogParams{
				UserID: pgtype.UUID{Bytes: p.UserID, Valid: true}, Action: "regular_guests_listed", Target: target,
			}); err != nil {
				return err
			}
		}
		rs, err := q.ListRegularGuests(ctx, db.ListRegularGuestsParams{
			HouseID: uuidOrNull(f.HouseID), Active: boolOrNull(f.Active),
			AfterCreatedAt: afterAt, AfterID: afterID, PageSize: int32(f.Limit) + 1,
		})
		for _, r := range rs {
			rows = append(rows, row(db.GetRegularGuestRow(r)))
		}
		return err
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("list regular guests: %w", err)
	}
	res := ListResult{Data: []Guest{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, toGuest(r))
	}
	return res, nil
}

// Update changes a regular guest of the owner's own house. Another house's entry looks exactly like a missing one.
func (s *Service) Update(ctx context.Context, p auth.Principal, id uuid.UUID, in UpdateInput) (Guest, error) {
	var out Guest
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		cur, err := q.GetRegularGuest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ownsHouse(p, cur.HouseID)) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get regular guest: %w", err)
		}
		// Turning an entry back on counts against the cap like adding a new one.
		if in.Active != nil && *in.Active && !cur.Active {
			if err := checkCap(ctx, q, cur.HouseID); err != nil {
				return err
			}
		}
		if _, err := q.UpdateRegularGuest(ctx, db.UpdateRegularGuestParams{
			ID: id, GuestName: textOrNull(in.GuestName), Plate: textOrNull(in.Plate), ClearPlate: in.ClearPlate,
			Category: textOrNull(in.Category), Active: boolOrNull(in.Active),
		}); err != nil {
			return fmt.Errorf("update regular guest: %w", err)
		}
		g, err := q.GetRegularGuest(ctx, id)
		if err != nil {
			return fmt.Errorf("reload regular guest: %w", err)
		}
		out = toGuest(row(g))
		return audit(ctx, q, p.UserID, "regular_guest_updated", id)
	})
	if err != nil {
		return Guest{}, wrap(err, "update")
	}
	return out, nil
}

// Delete removes a regular guest of the owner's own house for good. Visits already made from it stay.
func (s *Service) Delete(ctx context.Context, p auth.Principal, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		cur, err := q.GetRegularGuest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !ownsHouse(p, cur.HouseID)) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get regular guest: %w", err)
		}
		if _, err := q.DeleteRegularGuest(ctx, id); err != nil {
			return fmt.Errorf("delete regular guest: %w", err)
		}
		return audit(ctx, q, p.UserID, "regular_guest_deleted", id)
	})
	return wrap(err, "delete")
}

func checkCap(ctx context.Context, q *db.Queries, houseID uuid.UUID) error {
	n, err := q.CountActiveRegularGuests(ctx, houseID)
	if err != nil {
		return fmt.Errorf("count active regular guests: %w", err)
	}
	if n >= MaxActivePerHouse {
		return ErrLimitReached
	}
	return nil
}

// ownsHouse is the service-level house check, a second layer on top of row-level security.
func ownsHouse(p auth.Principal, houseID uuid.UUID) bool {
	return p.HouseID != nil && *p.HouseID == houseID
}

// row is the shared shape of every regular-guest query result (identical columns).
type row = db.GetRegularGuestRow

func toGuest(r row) Guest {
	g := Guest{
		ID:        r.ID,
		House:     HouseRef{ID: r.HouseID, Block: r.Block, Number: r.Number},
		GuestName: r.GuestName,
		Category:  r.Category,
		Active:    r.Active,
		CreatedAt: r.CreatedAt.In(httpx.WIB),
		UpdatedAt: r.UpdatedAt.In(httpx.WIB),
	}
	if r.Plate.Valid {
		g.Plate = &r.Plate.String
	}
	return g
}

// audit records the action against the entry's id, never its name or plate.
func audit(ctx context.Context, q *db.Queries, actor uuid.UUID, action string, id uuid.UUID) error {
	return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: action, Target: "regular:" + id.String(),
	})
}

func wrap(err error, op string) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrNotFound, ErrLimitReached, ErrNoHouse} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("%s regular guest: %w", op, err)
}

func textOrNull(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func boolOrNull(b *bool) pgtype.Bool {
	if b == nil {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: *b, Valid: true}
}

func uuidOrNull(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}
