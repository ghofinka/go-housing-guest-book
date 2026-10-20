package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidRefresh     = errors.New("invalid refresh token")
)

// UserInfo is the public view of an account returned with a token pair.
type UserInfo struct {
	ID      uuid.UUID  `json:"id"`
	Name    string     `json:"name"`
	Role    string     `json:"role"`
	HouseID *uuid.UUID `json:"house_id"`
}

// TokenPair is the result of a successful login or refresh.
type TokenPair struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	TokenType    string   `json:"token_type"`
	ExpiresIn    int      `json:"expires_in"`
	User         UserInfo `json:"user"`

	newTokenID uuid.UUID // id of the stored refresh token, used to link rotations
}

// Service implements login, refresh and logout.
type Service struct {
	pool       *pgxpool.Pool
	tokens     *TokenIssuer
	refreshTTL time.Duration
	// dummyHash is verified when the email is unknown so response time does not
	// reveal which emails exist.
	dummyHash string
}

func NewService(pool *pgxpool.Pool, tokens *TokenIssuer, refreshTTL time.Duration) (*Service, error) {
	dummy, err := HashPassword("not-a-real-password")
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, tokens: tokens, refreshTTL: refreshTTL, dummyHash: dummy}, nil
}

// Login verifies credentials and issues a new token family. Every outcome is audited.
func (s *Service) Login(ctx context.Context, email, password string) (TokenPair, error) {
	q := db.New(s.pool)
	user, err := q.GetUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = VerifyPassword(password, s.dummyHash)
		s.audit(ctx, q, pgtype.UUID{}, "login_failed", "unknown_email")
		return TokenPair{}, ErrInvalidCredentials
	}
	if err != nil {
		return TokenPair{}, fmt.Errorf("get user: %w", err)
	}
	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		return TokenPair{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok || !user.Active {
		s.audit(ctx, q, pgtype.UUID{Bytes: user.ID, Valid: true}, "login_failed", "user:"+user.ID.String())
		return TokenPair{}, ErrInvalidCredentials
	}

	var pair TokenPair
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		var err error
		pair, err = s.issue(ctx, tq, user, uuid.New())
		if err != nil {
			return err
		}
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: user.ID, Valid: true},
			Action: "login", Target: "user:" + user.ID.String(),
		})
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("login tx: %w", err)
	}
	return pair, nil
}

// Refresh rotates a refresh token. Presenting an already-revoked token revokes
// its whole family (reuse detection).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	var pair TokenPair
	var reuse bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		rt, err := tq.GetRefreshTokenForUpdate(ctx, hashRefreshToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidRefresh
		}
		if err != nil {
			return fmt.Errorf("get refresh token: %w", err)
		}
		uid := pgtype.UUID{Bytes: rt.UserID, Valid: true}
		if rt.RevokedAt.Valid && !rt.ReplacedBy.Valid {
			// Revoked by logout or a family kill, not rotated: reject without
			// re-flagging reuse (that would double-audit legitimate descendants).
			return ErrInvalidRefresh
		}
		if rt.RevokedAt.Valid {
			reuse = true
			if err := tq.RevokeRefreshTokenFamily(ctx, rt.FamilyID); err != nil {
				return fmt.Errorf("revoke family: %w", err)
			}
			return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
				UserID: uid, Action: "refresh_reuse_detected", Target: "user:" + rt.UserID.String(),
			})
		}
		if !rt.ExpiresAt.After(time.Now()) {
			return ErrInvalidRefresh
		}
		user, err := tq.GetUserByID(ctx, rt.UserID)
		if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if !user.Active {
			if err := tq.RevokeRefreshTokenFamily(ctx, rt.FamilyID); err != nil {
				return fmt.Errorf("revoke family: %w", err)
			}
			reuse = true // commit the revocation, then report invalid
			return nil
		}
		pair, err = s.issue(ctx, tq, user, rt.FamilyID)
		if err != nil {
			return err
		}
		// Link old -> new: the new row is the most recent in this family.
		return tq.RevokeRefreshToken(ctx, db.RevokeRefreshTokenParams{
			ID: rt.ID, ReplacedBy: pgtype.UUID{Bytes: pair.newTokenID, Valid: true},
		})
	})
	if err != nil {
		return TokenPair{}, err
	}
	if reuse {
		return TokenPair{}, ErrInvalidRefresh
	}
	return pair, nil
}

// Logout revokes the token's family. It is idempotent: unknown tokens succeed silently.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tq := db.New(tx)
		rt, err := tq.GetRefreshTokenForUpdate(ctx, hashRefreshToken(refreshToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get refresh token: %w", err)
		}
		if err := tq.RevokeRefreshTokenFamily(ctx, rt.FamilyID); err != nil {
			return fmt.Errorf("revoke family: %w", err)
		}
		return tq.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: rt.UserID, Valid: true},
			Action: "logout", Target: "user:" + rt.UserID.String(),
		})
	})
}

// Me returns the current account for an authenticated principal.
func (s *Service) Me(ctx context.Context, p Principal) (UserInfo, error) {
	user, err := db.New(s.pool).GetUserByID(ctx, p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserInfo{}, ErrInvalidCredentials
	}
	if err != nil {
		return UserInfo{}, fmt.Errorf("get user: %w", err)
	}
	if !user.Active {
		return UserInfo{}, ErrInvalidCredentials
	}
	return userInfo(user), nil
}

// issue creates an access token and a stored refresh token in the given family.
func (s *Service) issue(ctx context.Context, q *db.Queries, user db.User, family uuid.UUID) (TokenPair, error) {
	info := userInfo(user)
	access, err := s.tokens.Issue(Principal{UserID: user.ID, Role: user.Role, HouseID: info.HouseID})
	if err != nil {
		return TokenPair{}, err
	}
	refresh, hash, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, err
	}
	id, err := q.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		UserID: user.ID, FamilyID: family, TokenHash: hash,
		ExpiresAt: time.Now().Add(s.refreshTTL),
	})
	if err != nil {
		return TokenPair{}, fmt.Errorf("store refresh token: %w", err)
	}
	return TokenPair{
		AccessToken: access, RefreshToken: refresh, TokenType: "Bearer",
		ExpiresIn: int(s.tokens.TTL().Seconds()), User: info, newTokenID: id,
	}, nil
}

// audit records a failed-login event outside any transaction. A failure to
// write it must not turn a 401 into a 500, so the error is only logged by the caller's logger.
func (s *Service) audit(ctx context.Context, q *db.Queries, uid pgtype.UUID, action, target string) {
	_ = q.InsertAuditLog(ctx, db.InsertAuditLogParams{UserID: uid, Action: action, Target: target})
}

func userInfo(u db.User) UserInfo {
	info := UserInfo{ID: u.ID, Name: u.Name, Role: u.Role}
	if u.HouseID.Valid {
		hid := uuid.UUID(u.HouseID.Bytes)
		info.HouseID = &hid
	}
	return info
}
