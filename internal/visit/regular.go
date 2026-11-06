package visit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

// Reasons a regular guest (PRD C2) cannot be used at check-in. The handler answers 422: on regular_guest_id
// for the first, on house_id for the second.
var (
	ErrRegularGuestInvalid  = errors.New("regular guest not found or inactive")
	ErrRegularHouseMismatch = errors.New("regular guest belongs to another house")
)

// useRegular resolves in.RegularGuestID inside the check-in transaction. A guard may pick an entry of any
// house (row-level security lets the guard role read them all). The house, name, plate and category the
// guard did not give default from the entry. It returns the entry's id to link the visit to it.
func useRegular(ctx context.Context, q *db.Queries, in *CheckInInput) (pgtype.UUID, error) {
	g, err := q.GetRegularGuest(ctx, *in.RegularGuestID)
	// An inactive entry is refused like a missing one: the owner switched it off on purpose.
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !g.Active) {
		return pgtype.UUID{}, ErrRegularGuestInvalid
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("get regular guest: %w", err)
	}
	if in.HouseID != uuid.Nil && in.HouseID != g.HouseID {
		return pgtype.UUID{}, ErrRegularHouseMismatch
	}
	in.HouseID = g.HouseID
	if in.GuestName == nil {
		name := g.GuestName
		in.GuestName = &name
	}
	if in.VehiclePlate == nil && g.Plate.Valid {
		plate := g.Plate.String
		in.VehiclePlate = &plate
	}
	if in.Category == "" {
		in.Category = g.Category
	}
	return pgtype.UUID{Bytes: g.ID, Valid: true}, nil
}
