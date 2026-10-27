// Package audit reads the audit log (M11). Rows are written by the services that
// make or observe a change, in the same transaction; this package only reads, and
// only for admins. The application's database role has INSERT and SELECT on the
// table and nothing else, so no code path here or elsewhere can edit or delete rows.
package audit

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

const (
	defaultLimit = 50
	maxLimit     = 200
	maxActionLen = 64
	maxTargetLen = 200
)

// Entry is one audit row. UserName is empty for events with no known actor
// (for example a failed login for an unknown email).
type Entry struct {
	ID        int64      `json:"id"`
	UserID    *uuid.UUID `json:"user_id"`
	UserName  *string    `json:"user_name"`
	Action    string     `json:"action"`
	Target    string     `json:"target"`
	CreatedAt time.Time  `json:"created_at"`
}

type Filter struct {
	UserID       *uuid.UUID
	Action       string
	TargetPrefix string
	From, Before *time.Time
	Cursor       string
	Limit        int
}

type Result struct {
	Data       []Entry `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

var ErrBadCursor = errors.New("invalid cursor")

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) List(ctx context.Context, f Filter) (Result, error) {
	params := db.ListAuditLogsParams{PageSize: int32(f.Limit) + 1} // one extra row tells us if another page exists
	if f.UserID != nil {
		params.UserID = pgtype.UUID{Bytes: *f.UserID, Valid: true}
	}
	if f.Action != "" {
		params.Action = pgtype.Text{String: f.Action, Valid: true}
	}
	if f.TargetPrefix != "" {
		params.TargetPrefix = pgtype.Text{String: f.TargetPrefix, Valid: true}
	}
	if f.From != nil {
		params.FromAt = pgtype.Timestamptz{Time: *f.From, Valid: true}
	}
	if f.Before != nil {
		params.BeforeAt = pgtype.Timestamptz{Time: *f.Before, Valid: true}
	}
	if f.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		id, perr := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || perr != nil || id <= 0 {
			return Result{}, ErrBadCursor
		}
		params.BeforeID = pgtype.Int8{Int64: id, Valid: true}
	}

	rows, err := db.New(s.pool).ListAuditLogs(ctx, params)
	if err != nil {
		return Result{}, fmt.Errorf("list audit logs: %w", err)
	}
	res := Result{Data: []Entry{}}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(rows[len(rows)-1].ID, 10)))
		res.NextCursor = &c
	}
	for _, r := range rows {
		e := Entry{ID: r.ID, Action: r.Action, Target: r.Target, CreatedAt: r.CreatedAt.In(httpx.WIB)}
		if r.UserID.Valid {
			id := uuid.UUID(r.UserID.Bytes)
			e.UserID = &id
		}
		if r.UserName.Valid {
			e.UserName = &r.UserName.String
		}
		res.Data = append(res.Data, e)
	}
	return res, nil
}

// Handler exposes GET /v1/audit-logs, admin only. There is deliberately no write route.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /v1/audit-logs", h.authn.Require(http.HandlerFunc(h.list), auth.RoleAdmin))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Action: q.Get("action"), TargetPrefix: q.Get("target"), Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if v := q.Get("user_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["user_id"] = "user_id tidak valid."
		}
		f.UserID = &id
	}
	if utf8.RuneCountInString(f.Action) > maxActionLen {
		fields["action"] = "action terlalu panjang."
	}
	if utf8.RuneCountInString(f.TargetPrefix) > maxTargetLen {
		fields["target"] = "target terlalu panjang."
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			fields["limit"] = "limit harus 1 sampai " + strconv.Itoa(maxLimit) + "."
		} else {
			f.Limit = n
		}
	}
	// from/to: WIB calendar days, both inclusive.
	if d, ok := httpx.ParseDay(q.Get("from"), "from", fields); ok {
		f.From = &d
	}
	if d, ok := httpx.ParseDay(q.Get("to"), "to", fields); ok {
		end := d.AddDate(0, 0, 1)
		f.Before = &end
	}
	if f.From != nil && f.Before != nil && !f.From.Before(*f.Before) {
		fields["to"] = "Tanggal akhir tidak boleh sebelum tanggal awal."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}

	res, err := h.svc.List(r.Context(), f)
	if errors.Is(err, ErrBadCursor) {
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
		return
	}
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}
