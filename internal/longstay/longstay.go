// Package longstay holds the admin-editable limit behind the long-stay flag (PRD S7): a guest still
// inside after this many hours is marked overstay on every visit. The flag itself is computed in SQL
// (the long_stay_hours() function, migration 0008); this package only reads and changes the setting.
package longstay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

const (
	// SettingHours is the settings key. The SQL function long_stay_hours() reads the same key and
	// default, so keep them in step with migrations/0008_long_stay.up.sql.
	SettingHours = "long_stay_hours"

	DefaultHours = 12
	MinHours     = 1
	MaxHours     = 168 // one week
)

// Settings is the long-stay configuration.
type Settings struct {
	Hours int `json:"long_stay_hours"`
}

// Validate returns field problems in plain Indonesian, keyed by field name.
func (s Settings) Validate() map[string]string {
	if s.Hours < MinHours || s.Hours > MaxHours {
		return map[string]string{SettingHours: fmt.Sprintf("Harus antara %d dan %d jam.", MinHours, MaxHours)}
	}
	return nil
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Get returns the stored limit, or the default when nothing was saved. A stored value that is not a
// valid number is reported as an error rather than silently replaced, so the admin sees it; the SQL
// function still falls back to the default so visit lists keep working meanwhile.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	return get(ctx, db.New(s.pool))
}

func get(ctx context.Context, q *db.Queries) (Settings, error) {
	v, err := q.GetSetting(ctx, SettingHours)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{Hours: DefaultHours}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("get long-stay setting: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return Settings{}, fmt.Errorf("setting %s is not a number", SettingHours)
	}
	out := Settings{Hours: n}
	if problems := out.Validate(); len(problems) > 0 {
		return Settings{}, fmt.Errorf("stored long-stay setting is invalid: %v", problems)
	}
	return out, nil
}

// Update stores a new limit (already validated by the caller) and audits the change.
func (s *Service) Update(ctx context.Context, actor uuid.UUID, in Settings) (Settings, error) {
	err := db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
		// The previous value is only for the audit line, so a corrupt row must not block fixing it.
		old := Settings{Hours: DefaultHours}
		if cur, err := get(ctx, q); err == nil {
			old = cur
		}
		if err := q.UpsertSetting(ctx, db.UpsertSettingParams{Key: SettingHours, Value: strconv.Itoa(in.Hours)}); err != nil {
			return fmt.Errorf("save %s: %w", SettingHours, err)
		}
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true},
			Action: "long_stay_settings_changed",
			Target: fmt.Sprintf("%s:%d->%d", SettingHours, old.Hours, in.Hours),
		})
	})
	if err != nil {
		return Settings{}, fmt.Errorf("update long-stay setting: %w", err)
	}
	return in, nil
}

// Handler exposes GET and PUT /v1/settings/long-stay, admin only.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("GET /v1/settings/long-stay", admin(h.get))
	mux.Handle("PUT /v1/settings/long-stay", admin(h.put))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.svc.Get(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, cfg)
}

type putRequest struct {
	Hours *int `json:"long_stay_hours"`
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var req putRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.Hours == nil {
		httpx.ValidationError(w, map[string]string{SettingHours: "Wajib diisi."})
		return
	}
	in := Settings{Hours: *req.Hours}
	if problems := in.Validate(); len(problems) > 0 {
		httpx.ValidationError(w, problems)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := h.svc.Update(r.Context(), p.UserID, in)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("long-stay settings request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}
