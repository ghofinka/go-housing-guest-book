// Package shiftnote lets a guard leave a note for the next shift in the app (PRD C5). Guards and admins read
// every note and add new ones; only an admin deletes. A note is free text, so the retention job deletes it
// after shift_note_retention_days (default 30), and the audit log records only that a note exists, never its text.
package shiftnote

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

var ErrNotFound = errors.New("shift note not found")

// MaxBodyLen is the longest note, in characters.
const MaxBodyLen = 1000

type Author struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// Note is one shift note as the API shows it.
type Note struct {
	ID        uuid.UUID `json:"id"`
	Author    Author    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ListFilter bounds the notes by creation time: From is included, Before is excluded (both optional).
type ListFilter struct {
	From, Before *time.Time
	Cursor       string
	Limit        int
}

type ListResult struct {
	Data       []Note  `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create adds a note written by the caller.
func (s *Service) Create(ctx context.Context, p auth.Principal, body string) (Note, error) {
	var out Note
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		id, err := q.CreateShiftNote(ctx, db.CreateShiftNoteParams{AuthorID: p.UserID, Body: body})
		if err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		n, err := q.GetShiftNote(ctx, id)
		if err != nil {
			return fmt.Errorf("reload: %w", err)
		}
		out = toNote(n.ID, n.AuthorID, n.AuthorName, n.Body, n.CreatedAt)
		return audit(ctx, q, p.UserID, "shift_note_created", id)
	})
	if err != nil {
		return Note{}, fmt.Errorf("create shift note: %w", err)
	}
	return out, nil
}

// List returns notes newest first. Reading is not audited, like the guard's visit list: the screen is opened
// at every shift change and the log would fill with entries that say nothing.
func (s *Service) List(ctx context.Context, p auth.Principal, f ListFilter) (ListResult, error) {
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
	var rows []db.ListShiftNotesRow
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		var err error
		rows, err = q.ListShiftNotes(ctx, db.ListShiftNotesParams{
			FromAt: timeOrNull(f.From), BeforeAt: timeOrNull(f.Before),
			AfterCreatedAt: afterAt, AfterID: afterID, PageSize: int32(f.Limit) + 1,
		})
		return err
	})
	if err != nil {
		return ListResult{}, fmt.Errorf("list shift notes: %w", err)
	}
	res := ListResult{Data: []Note{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, toNote(r.ID, r.AuthorID, r.AuthorName, r.Body, r.CreatedAt))
	}
	return res, nil
}

// Delete removes a note for good. Only an admin may (the route and row-level security both say so).
func (s *Service) Delete(ctx context.Context, p auth.Principal, id uuid.UUID) error {
	err := db.InTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		if _, err := q.DeleteShiftNote(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		return audit(ctx, q, p.UserID, "shift_note_deleted", id)
	})
	if errors.Is(err, ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("delete shift note: %w", err)
	}
	return nil
}

func toNote(id, authorID uuid.UUID, authorName, body string, at time.Time) Note {
	return Note{ID: id, Author: Author{ID: authorID, Name: authorName}, Body: body, CreatedAt: at.In(httpx.WIB)}
}

// audit records the action against the note's id, never its text.
func audit(ctx context.Context, q *db.Queries, actor uuid.UUID, action string, id uuid.UUID) error {
	return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
		UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: action, Target: "shift_note:" + id.String(),
	})
}

func timeOrNull(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
