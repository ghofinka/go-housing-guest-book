// Package house manages the master list of houses (M10).
package house

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

var (
	ErrNotFound = errors.New("house not found")
	ErrConflict = errors.New("house already exists")
)

// maxFieldLen bounds block and number.
const maxFieldLen = 20

// House is the API view of a house.
type House struct {
	ID        uuid.UUID `json:"id"`
	Block     string    `json:"block"`
	Number    string    `json:"number"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toHouse(h db.House) House {
	return House{ID: h.ID, Block: h.Block, Number: h.Number, Active: h.Active, CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt}
}

// UpdateInput holds the optional fields of a PATCH; nil means "leave unchanged".
type UpdateInput struct {
	Block  *string
	Number *string
	Active *bool
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// normalize trims the fields and upper-cases the block so "a" and "A" cannot coexist.
func normalizeBlock(s string) string  { return strings.ToUpper(strings.TrimSpace(s)) }
func normalizeNumber(s string) string { return strings.TrimSpace(s) }

// validate returns field-level problems for a block/number pair (already normalized).
func validateFields(block, number *string) map[string]string {
	fields := map[string]string{}
	if block != nil && utf8.RuneCountInString(*block) > maxFieldLen {
		fields["block"] = fmt.Sprintf("Blok maksimal %d karakter.", maxFieldLen)
	}
	if number != nil {
		switch n := utf8.RuneCountInString(*number); {
		case n == 0:
			fields["number"] = "Nomor rumah wajib diisi."
		case n > maxFieldLen:
			fields["number"] = fmt.Sprintf("Nomor rumah maksimal %d karakter.", maxFieldLen)
		}
	}
	return fields
}

func (s *Service) Create(ctx context.Context, actor uuid.UUID, block, number string) (House, error) {
	var out House
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		h, err := tq.CreateHouse(ctx, db.CreateHouseParams{Block: block, Number: number})
		if err != nil {
			return err
		}
		out = toHouse(h)
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: "house_created", Target: "house:" + h.ID.String(),
		})
	})
	if db.IsUniqueViolation(err) {
		return House{}, ErrConflict
	}
	if err != nil {
		return House{}, fmt.Errorf("create house: %w", err)
	}
	return out, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (House, error) {
	h, err := db.New(s.pool).GetHouse(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return House{}, ErrNotFound
	}
	if err != nil {
		return House{}, fmt.Errorf("get house: %w", err)
	}
	return toHouse(h), nil
}

func (s *Service) List(ctx context.Context, includeInactive bool) ([]House, error) {
	rows, err := db.New(s.pool).ListHouses(ctx, includeInactive)
	if err != nil {
		return nil, fmt.Errorf("list houses: %w", err)
	}
	out := make([]House, len(rows))
	for i, h := range rows {
		out[i] = toHouse(h)
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, actor, id uuid.UUID, in UpdateInput) (House, error) {
	var out House
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		h, err := tq.UpdateHouse(ctx, db.UpdateHouseParams{
			ID:     id,
			Block:  textOrNull(in.Block),
			Number: textOrNull(in.Number),
			Active: boolOrNull(in.Active),
		})
		if err != nil {
			return err
		}
		out = toHouse(h)
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: "house_updated", Target: "house:" + h.ID.String(),
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return House{}, ErrNotFound
	}
	if db.IsUniqueViolation(err) {
		return House{}, ErrConflict
	}
	if err != nil {
		return House{}, fmt.Errorf("update house: %w", err)
	}
	return out, nil
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
