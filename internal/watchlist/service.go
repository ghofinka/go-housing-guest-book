// Package watchlist manages the plates and names that need special attention at the gate (PRD C1).
//
// Only an admin reads or changes the list. Check-in learns about a match through the watchlist_check
// SQL function (see internal/visit), which answers with one note and never exposes the list, so a guard
// is told "this guest needs attention" without ever being able to read who else is on it. A match is a
// flag, not a block: the guard decides at the gate.
package watchlist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

var (
	ErrNotFound = errors.New("watchlist entry not found")
	ErrExists   = errors.New("an active entry with that plate or name already exists")
)

const (
	KindPlate = "plate"
	KindName  = "name"

	maxPlateLen = 12
	maxNameLen  = 100
	minValueLen = 2
	maxNoteLen  = 200
)

// Entry is one watchlist entry as an admin sees it.
type Entry struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Value     string    `json:"value"`
	Note      string    `json:"note"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ListFilter struct {
	Kind   string // "", "plate" or "name"
	Active *bool
	Cursor string
	Limit  int
}

type ListResult struct {
	Data       []Entry `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// NormalizePlate returns a plate in upper case without spaces. It accepts only letters and digits, and
// the SQL function applies the same rule to the plate it is asked about.
func NormalizePlate(s string) (string, bool) {
	p := strings.ToUpper(strings.Join(strings.Fields(s), ""))
	if n := utf8.RuneCountInString(p); n < minValueLen || n > maxPlateLen {
		return p, false
	}
	for _, r := range p {
		if r >= unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return p, false
		}
	}
	return p, true
}

// NormalizeName returns a name in lower case with single spaces, as the SQL function does.
func NormalizeName(s string) (string, bool) {
	n := strings.ToLower(strings.Join(strings.Fields(s), " "))
	c := utf8.RuneCountInString(n)
	return n, c >= minValueLen && c <= maxNameLen
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create adds an entry. The value is already normalized by the caller.
func (s *Service) Create(ctx context.Context, actor auth.Principal, kind, value, note string) (Entry, error) {
	var out Entry
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		id, err := q.CreateWatchlistEntry(ctx, db.CreateWatchlistEntryParams{Kind: kind, Value: value, Note: note, CreatedBy: actor.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrExists
		}
		if err != nil {
			return fmt.Errorf("create watchlist entry: %w", err)
		}
		row, err := q.GetWatchlistEntry(ctx, id)
		if err != nil {
			return fmt.Errorf("reload watchlist entry: %w", err)
		}
		out = toEntry(row)
		return audit(ctx, q, actor, "watchlist_created", id.String())
	})
	if err != nil {
		return Entry{}, wrap(err, "create")
	}
	return out, nil
}

// List returns entries newest first.
func (s *Service) List(ctx context.Context, actor auth.Principal, f ListFilter) (ListResult, error) {
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
	var rows []db.ListWatchlistEntriesRow
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		if err := audit(ctx, q, actor, "watchlist_listed", "all"); err != nil {
			return err
		}
		var err error
		rows, err = q.ListWatchlistEntries(ctx, db.ListWatchlistEntriesParams{
			Kind: textOrNull(f.Kind), Active: boolOrNull(f.Active),
			AfterCreatedAt: afterAt, AfterID: afterID, PageSize: int32(f.Limit) + 1,
		})
		return err
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("list watchlist: %w", err)
	}
	res := ListResult{Data: []Entry{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, Entry{ID: r.ID, Kind: r.Kind, Value: r.Value, Note: r.Note, Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return res, nil
}

// Update changes an entry's note and/or active flag; a nil field stays as it is.
func (s *Service) Update(ctx context.Context, actor auth.Principal, id uuid.UUID, note *string, active *bool) (Entry, error) {
	var out Entry
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		_, err := q.UpdateWatchlistEntry(ctx, db.UpdateWatchlistEntryParams{ID: id, Note: textOrNullPtr(note), Active: boolOrNull(active)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if db.IsUniqueViolation(err) {
			return ErrExists
		}
		if err != nil {
			return fmt.Errorf("update watchlist entry: %w", err)
		}
		row, err := q.GetWatchlistEntry(ctx, id)
		if err != nil {
			return fmt.Errorf("reload watchlist entry: %w", err)
		}
		out = toEntry(row)
		return audit(ctx, q, actor, "watchlist_updated", id.String())
	})
	if err != nil {
		return Entry{}, wrap(err, "update")
	}
	return out, nil
}

// Delete removes an entry for good, so a plate or name can be erased on request.
func (s *Service) Delete(ctx context.Context, actor auth.Principal, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		if _, err := q.DeleteWatchlistEntry(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("delete watchlist entry: %w", err)
		}
		return audit(ctx, q, actor, "watchlist_deleted", id.String())
	})
	return wrap(err, "delete")
}

func toEntry(r db.GetWatchlistEntryRow) Entry {
	return Entry{ID: r.ID, Kind: r.Kind, Value: r.Value, Note: r.Note, Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// audit records the action against the entry's id. The target never carries the plate, the name or the
// note: the audit log is read more widely than the list itself.
func audit(ctx context.Context, q *db.Queries, actor auth.Principal, action, id string) error {
	return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: pgtype.UUID{Bytes: actor.UserID, Valid: true}, Action: action, Target: "watchlist:" + id,
	})
}

func wrap(err error, op string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrExists) {
		return err
	}
	return fmt.Errorf("%s watchlist entry: %w", op, err)
}

func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textOrNullPtr(s *string) pgtype.Text {
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
