package visit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Pre-registration codes (PRD S2) are 8 characters from an alphabet without look-alikes
// (no I, O, 0, 1) so a guard can read one aloud or key it in without mistakes.
const (
	CodeLength   = 8
	CodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// MaxNameLen and NormalizePlate are shared with the prereg package so a pre-registration
// obeys the same rules as a check-in.
const MaxNameLen = maxNameLen

func NormalizePlate(s string) (string, bool) { return normalizePlate(s) }

// Reasons a code cannot be used at check-in. The handler maps all of them to a 422 on
// pre_registration_code (house mismatch on house_id).
var (
	ErrPreRegNotFound       = errors.New("pre-registration code not found")
	ErrPreRegUsed           = errors.New("pre-registration code already used")
	ErrPreRegCancelled      = errors.New("pre-registration code cancelled")
	ErrPreRegWrongDate      = errors.New("pre-registration code not valid today")
	ErrPreRegHouseMismatch  = errors.New("pre-registration belongs to another house")
	ErrVehiclePhotoRequired = errors.New("vehicle photo required with a plate")
)

// NormalizeCode upper-cases a typed or scanned code and drops spaces and hyphens, so
// "abcd-2345" and "ABCD 2345" are the same code. It reports whether the result is well formed.
func NormalizeCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r == ' ' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	code := b.String()
	if len(code) != CodeLength {
		return code, false
	}
	for _, r := range code {
		if !strings.ContainsRune(CodeAlphabet, r) {
			return code, false
		}
	}
	return code, true
}

// UsableOn reports whether a pre-registration for the calendar day expected can be used at now.
// A code is valid only on its expected date in WIB (the cluster's local day).
func UsableOn(expected, now time.Time) bool {
	y, m, d := now.In(httpx.WIB).Date()
	return expected.Year() == y && expected.Month() == m && expected.Day() == d
}

// useCode resolves in.PreRegistrationCode inside the check-in transaction. The row is locked
// until the visit is saved, so a code works once. It fills the house, name and plate the guard
// did not type and returns the pre-registration id to link and later mark as used.
func useCode(ctx context.Context, q *db.Queries, in *CheckInInput, now time.Time) (pgtype.UUID, error) {
	pr, err := q.LockPreRegistrationByCode(ctx, in.PreRegistrationCode)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrPreRegNotFound
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("lock pre-registration: %w", err)
	}
	switch pr.Status {
	case "used":
		return pgtype.UUID{}, ErrPreRegUsed
	case "cancelled":
		return pgtype.UUID{}, ErrPreRegCancelled
	}
	if !UsableOn(pr.ExpectedDate.Time, now) {
		return pgtype.UUID{}, ErrPreRegWrongDate
	}
	if in.HouseID != uuid.Nil && in.HouseID != pr.HouseID {
		return pgtype.UUID{}, ErrPreRegHouseMismatch
	}
	in.HouseID = pr.HouseID
	if in.GuestName == nil && pr.GuestName.Valid {
		in.GuestName = &pr.GuestName.String
	}
	if in.VehiclePlate == nil && pr.Plate.Valid {
		in.VehiclePlate = &pr.Plate.String
	}
	return pgtype.UUID{Bytes: pr.ID, Valid: true}, nil
}
