// Package user manages accounts (guards, house owners, admins) for M10.
package user

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
	ErrNotFound         = errors.New("user not found")
	ErrEmailTaken       = errors.New("email already used")
	ErrHouseInvalid     = errors.New("house missing or inactive")
	ErrSelfDeactivation = errors.New("cannot deactivate own account")
	ErrBadCursor        = httpx.ErrBadCursor
)

var validRoles = map[string]bool{"guard": true, "owner": true, "admin": true}

// User is the API view of an account. It never includes the password hash.
type User struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	HouseID   *uuid.UUID `json:"house_id"`
	Contact   *string    `json:"contact"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type CreateInput struct {
	Name     string
	Email    string
	Password string
	Role     string
	HouseID  *uuid.UUID
	Contact  *string
}

// UpdateInput holds the optional fields of a PATCH; nil means "leave unchanged".
type UpdateInput struct {
	Name     *string
	Contact  *string
	HouseID  *uuid.UUID
	Active   *bool
	Password *string
}

type ListFilter struct {
	Role    string
	HouseID *uuid.UUID
	Cursor  string
	Limit   int
}

type ListResult struct {
	Data       []User  `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, actor uuid.UUID, in CreateInput) (User, error) {
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	var out User
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		if in.HouseID != nil {
			if err := requireActiveHouse(ctx, tq, *in.HouseID); err != nil {
				return err
			}
		}
		u, err := tq.CreateUser(ctx, db.CreateUserParams{
			Name: in.Name, Email: in.Email, PasswordHash: hash, Role: in.Role,
			HouseID: uuidOrNull(in.HouseID), Contact: textOrNull(in.Contact),
		})
		if err != nil {
			return err
		}
		out = toUser(u)
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: "user_created", Target: "user:" + u.ID.String(),
		})
	})
	return out, mapErr(err, "create user")
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := db.New(s.pool).GetUserByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return toUser(u), nil
}

func (s *Service) List(ctx context.Context, f ListFilter) (ListResult, error) {
	params := db.ListUsersParams{PageSize: int32(f.Limit) + 1} // one extra row tells us if there is a next page
	if f.Role != "" {
		params.Role = pgtype.Text{String: f.Role, Valid: true}
	}
	params.HouseID = uuidOrNull(f.HouseID)
	if f.Cursor != "" {
		at, id, err := httpx.DecodeCursor(f.Cursor)
		if err != nil {
			return ListResult{}, ErrBadCursor
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: at, Valid: true}
		params.AfterID = pgtype.UUID{Bytes: id, Valid: true}
	}
	rows, err := db.New(s.pool).ListUsers(ctx, params)
	if err != nil {
		return ListResult{}, fmt.Errorf("list users: %w", err)
	}
	res := ListResult{Data: []User{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		last := rows[len(rows)-1]
		c := httpx.EncodeCursor(last.CreatedAt, last.ID)
		res.NextCursor = &c
	}
	for _, r := range rows {
		res.Data = append(res.Data, User{
			ID: r.ID, Name: r.Name, Email: r.Email, Role: r.Role, HouseID: uuidPtr(r.HouseID),
			Contact: textPtr(r.Contact), Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return res, nil
}

// Update changes an account. Deactivating it or resetting its password also
// revokes every refresh token so existing sessions end.
func (s *Service) Update(ctx context.Context, actor, id uuid.UUID, in UpdateInput) (User, error) {
	if in.Active != nil && !*in.Active && actor == id {
		return User{}, ErrSelfDeactivation
	}
	params := db.UpdateUserParams{
		ID: id, Name: textOrNull(in.Name), Contact: textOrNull(in.Contact), HouseID: uuidOrNull(in.HouseID),
	}
	if in.Active != nil {
		params.Active = pgtype.Bool{Bool: *in.Active, Valid: true}
	}
	if in.Password != nil {
		hash, err := auth.HashPassword(*in.Password)
		if err != nil {
			return User{}, err
		}
		params.PasswordHash = pgtype.Text{String: hash, Valid: true}
	}

	var out User
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		if in.HouseID != nil {
			cur, err := tq.GetUserByID(ctx, id)
			if err != nil {
				return err
			}
			if cur.Role != "owner" {
				return ErrHouseInvalid // only owners belong to a house
			}
			if err := requireActiveHouse(ctx, tq, *in.HouseID); err != nil {
				return err
			}
		}
		u, err := tq.UpdateUser(ctx, params)
		if err != nil {
			return err
		}
		out = toUser(u)
		action := "user_updated"
		if in.Active != nil && !*in.Active {
			action = "user_deactivated"
		}
		if (in.Active != nil && !*in.Active) || in.Password != nil {
			if err := tq.RevokeUserRefreshTokens(ctx, id); err != nil {
				return fmt.Errorf("revoke sessions: %w", err)
			}
		}
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true}, Action: action, Target: "user:" + id.String(),
		})
	})
	return out, mapErr(err, "update user")
}

func requireActiveHouse(ctx context.Context, q *db.Queries, id uuid.UUID) error {
	h, err := q.GetHouse(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !h.Active) {
		return ErrHouseInvalid
	}
	return err
}

func mapErr(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case db.IsUniqueViolation(err):
		return ErrEmailTaken
	case errors.Is(err, ErrHouseInvalid):
		return ErrHouseInvalid
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

func toUser(u db.User) User {
	return User{
		ID: u.ID, Name: u.Name, Email: u.Email, Role: u.Role, HouseID: uuidPtr(u.HouseID),
		Contact: textPtr(u.Contact), Active: u.Active, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func uuidPtr(u pgtype.UUID) *uuid.UUID {
	if !u.Valid {
		return nil
	}
	id := uuid.UUID(u.Bytes)
	return &id
}

func uuidOrNull(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
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
