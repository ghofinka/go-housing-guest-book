// Package report exports the visit log for a period as CSV or PDF (PRD S8). It is admin only, never
// includes photos, leaves out guest names and plates unless the admin asks for them, and records every
// export in the audit log (the scope and the number of rows, never a name or plate).
package report

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

const (
	// MaxPeriodDays is the longest period one report may cover.
	MaxPeriodDays = 93
	// MaxPDFRows caps a PDF, which is built in memory. A CSV is streamed and has no row cap.
	MaxPDFRows = 5000

	defaultPageSize = 200
)

// Params selects what goes into a report.
type Params struct {
	// From and To are calendar days in WIB (midnight), both inclusive, matched on check-in time.
	From, To time.Time
	HouseID  *uuid.UUID
	Category string // "" for all
	Status   string // "", "inside" or "exited"
	// IncludePersonal adds guest name, plate and note. Off by default: a report for a meeting rarely
	// needs them, and an export is the easiest way for personal data to leave the system.
	IncludePersonal bool
}

// Row is one visit as a report shows it.
type Row struct {
	CheckinAt     time.Time
	CheckoutAt    *time.Time
	MinutesInside *int // only once the guest has left
	House         string
	Category      string
	Status        string // inside or exited
	OwnerDecision string // pending, approved or rejected
	Overstay      bool
	// Personal fields; empty when the report does not include them or the record was anonymized.
	GuestName, Plate, Note string
}

type Service struct {
	pool     *pgxpool.Pool
	pageSize int32
	now      func() time.Time
}

// Option adjusts a Service.
type Option func(*Service)

// WithPageSize sets how many rows are read per query (default 200).
func WithPageSize(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.pageSize = int32(n)
		}
	}
}

func NewService(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool, pageSize: defaultPageSize, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// bounds returns the check-in time range [from, before). Before is the day after To in WIB, but never
// later than now, so the count taken before streaming and the rows streamed afterwards describe the
// same set even when guests keep arriving.
func (s *Service) bounds(p Params) (from, before time.Time) {
	before = p.To.AddDate(0, 0, 1)
	if now := s.now(); before.After(now) {
		before = now
	}
	return p.From, before
}

// Count returns how many visits the report would contain.
func (s *Service) Count(ctx context.Context, actor auth.Principal, p Params) (int, error) {
	from, before := s.bounds(p)
	var n int32
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		var err error
		n, err = q.CountVisits(ctx, db.CountVisitsParams{
			Status: textOrNull(p.Status), HouseID: uuidOrNull(p.HouseID), Category: textOrNull(p.Category),
			FromAt: from, BeforeAt: before,
		})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("count report rows: %w", err)
	}
	return int(n), nil
}

// Audit records that an export is about to happen. It is written before any data is sent, so a
// download that is cut off halfway is still on record. The target describes the scope and the row
// count only: never a name, plate or search text.
func (s *Service) Audit(ctx context.Context, actor auth.Principal, format string, p Params, rows int) error {
	target := fmt.Sprintf("format:%s from:%s to:%s personal:%t rows:%d",
		format, p.From.Format(time.DateOnly), p.To.Format(time.DateOnly), p.IncludePersonal, rows)
	if p.HouseID != nil {
		target += " house:" + p.HouseID.String()
	}
	if p.Category != "" {
		target += " category:" + p.Category
	}
	if p.Status != "" {
		target += " status:" + p.Status
	}
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor.UserID, Valid: true}, Action: "visits_exported", Target: target,
		})
	})
	if err != nil {
		return fmt.Errorf("audit export: %w", err)
	}
	return nil
}

// Each reads the report's rows page by page, newest first, and hands each page to fn. Every page is
// its own short transaction, so a slow download never holds one open.
func (s *Service) Each(ctx context.Context, actor auth.Principal, p Params, fn func(page []Row) error) error {
	from, before := s.bounds(p)
	var afterAt pgtype.Timestamptz
	var afterID pgtype.UUID
	for {
		var rows []db.ListVisitsRow
		err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
			var err error
			rows, err = q.ListVisits(ctx, db.ListVisitsParams{
				Status: textOrNull(p.Status), HouseID: uuidOrNull(p.HouseID), Category: textOrNull(p.Category),
				FromAt:         pgtype.Timestamptz{Time: from, Valid: true},
				BeforeAt:       pgtype.Timestamptz{Time: before, Valid: true},
				AfterCheckinAt: afterAt, AfterID: afterID, PageSize: s.pageSize,
			})
			return err
		})
		if err != nil {
			return fmt.Errorf("read report rows: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		page := make([]Row, len(rows))
		for i, r := range rows {
			page[i] = toRow(r, p.IncludePersonal)
		}
		if err := fn(page); err != nil {
			return err
		}
		if int32(len(rows)) < s.pageSize {
			return nil
		}
		last := rows[len(rows)-1]
		afterAt = pgtype.Timestamptz{Time: last.CheckinAt, Valid: true}
		afterID = pgtype.UUID{Bytes: last.ID, Valid: true}
	}
}

func toRow(r db.ListVisitsRow, personal bool) Row {
	out := Row{
		CheckinAt:     r.CheckinAt.In(httpx.WIB),
		House:         fmt.Sprintf("Blok %s No %s", r.Block, r.Number),
		Category:      r.Category,
		Status:        r.Status,
		OwnerDecision: r.OwnerDecision,
		Overstay:      r.Overstay,
	}
	if r.CheckoutAt.Valid {
		t := r.CheckoutAt.Time.In(httpx.WIB)
		out.CheckoutAt = &t
		m := int(t.Sub(r.CheckinAt).Minutes())
		out.MinutesInside = &m
	}
	if personal {
		out.GuestName, out.Plate, out.Note = r.GuestName.String, r.VehiclePlate.String, r.PurposeNote.String
	}
	return out
}

func textOrNull(s string) pgtype.Text {
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
