package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// SettingFastLane is the settings key for whether courier and ojol visits also notify the owner.
// It is off by default: couriers come all day and would flood an owner's inbox.
const SettingFastLane = "notify_fast_lane"

// Settings is what an admin can change.
type Settings struct {
	NotifyFastLane bool `json:"notify_fast_lane"`
}

// Status is the settings plus a read-only view of the queue.
type Status struct {
	Settings
	// Enabled is false when no mail server is configured: nothing is queued or sent.
	Enabled bool `json:"enabled"`
	Pending int  `json:"pending"`
	Sent    int  `json:"sent"`
	Failed  int  `json:"failed"`
	Skipped int  `json:"skipped"`
}

type SettingsService struct {
	pool    *pgxpool.Pool
	enabled bool
}

func NewSettingsService(pool *pgxpool.Pool, enabled bool) *SettingsService {
	return &SettingsService{pool: pool, enabled: enabled}
}

// FastLaneEnabled reads the setting inside the caller's transaction (check-in uses it). A missing or
// unreadable value means off.
func FastLaneEnabled(ctx context.Context, q *db.Queries) (bool, error) {
	v, err := q.GetSetting(ctx, SettingFastLane)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get %s: %w", SettingFastLane, err)
	}
	return v == "true", nil
}

func (s *SettingsService) Status(ctx context.Context) (Status, error) {
	q := db.New(s.pool)
	on, err := FastLaneEnabled(ctx, q)
	if err != nil {
		return Status{}, err
	}
	out := Status{Settings: Settings{NotifyFastLane: on}, Enabled: s.enabled}
	counts, err := q.CountNotificationsByStatus(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("count notifications: %w", err)
	}
	for _, c := range counts {
		switch c.Status {
		case "pending":
			out.Pending = int(c.Total)
		case "sent":
			out.Sent = int(c.Total)
		case "failed":
			out.Failed = int(c.Total)
		case "skipped":
			out.Skipped = int(c.Total)
		}
	}
	return out, nil
}

// Update stores the settings (already validated) and audits the change.
func (s *SettingsService) Update(ctx context.Context, actor uuid.UUID, in Settings) error {
	return db.InTx(ctx, s.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
		old, err := FastLaneEnabled(ctx, q)
		if err != nil {
			return err
		}
		if err := q.UpsertSetting(ctx, db.UpsertSettingParams{Key: SettingFastLane, Value: fmt.Sprint(in.NotifyFastLane)}); err != nil {
			return fmt.Errorf("save %s: %w", SettingFastLane, err)
		}
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor, Valid: true},
			Action: "notification_settings_changed",
			Target: fmt.Sprintf("%s:%t->%t", SettingFastLane, old, in.NotifyFastLane),
		})
	})
}

// Handler exposes GET and PUT /v1/settings/notifications, admin only.
type Handler struct {
	svc   *SettingsService
	authn *auth.Authenticator
}

func NewHandler(svc *SettingsService, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("GET /v1/settings/notifications", admin(h.get))
	mux.Handle("PUT /v1/settings/notifications", admin(h.put))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	st, err := h.svc.Status(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NotifyFastLane *bool `json:"notify_fast_lane"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.NotifyFastLane == nil {
		httpx.ValidationError(w, map[string]string{SettingFastLane: "Wajib diisi (true atau false)."})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.svc.Update(r.Context(), p.UserID, Settings{NotifyFastLane: *req.NotifyFastLane}); err != nil {
		h.serverError(w, r, err)
		return
	}
	st, err := h.svc.Status(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, st)
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("notification settings request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}
