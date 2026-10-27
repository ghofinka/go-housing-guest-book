package retention

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

const recentRuns = 10

// Handler exposes the admin-only retention endpoints.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("GET /v1/settings/retention", admin(h.get))
	mux.Handle("PUT /v1/settings/retention", admin(h.put))
	mux.Handle("POST /v1/retention/run", admin(h.run))
}

type settingsResponse struct {
	Settings
	RecentRuns []RunInfo `json:"recent_runs"`
}

type putRequest struct {
	PhotoDays        *int `json:"photo_retention_days"`
	PersonalDataDays *int `json:"personal_data_retention_days"`
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	h.writeSettings(w, r, http.StatusOK, nil)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	var req putRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	if req.PhotoDays == nil {
		fields[SettingPhotoDays] = "Wajib diisi."
	}
	if req.PersonalDataDays == nil {
		fields[SettingPersonalDays] = "Wajib diisi."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	in := Settings{PhotoDays: *req.PhotoDays, PersonalDataDays: *req.PersonalDataDays}
	if problems := in.Validate(); len(problems) > 0 {
		httpx.ValidationError(w, problems)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	saved, err := h.svc.UpdateSettings(r.Context(), p.UserID, in)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.writeSettings(w, r, http.StatusOK, &saved)
}

func (h *Handler) writeSettings(w http.ResponseWriter, r *http.Request, status int, known *Settings) {
	var cfg Settings
	if known != nil {
		cfg = *known
	} else {
		var err error
		if cfg, err = h.svc.Settings(r.Context()); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	runs, err := h.svc.Runs(r.Context(), recentRuns)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	httpx.JSON(w, status, settingsResponse{Settings: cfg, RecentRuns: runs})
}

// run triggers a pass now. ?dry_run=true reports what would be deleted without changing anything.
func (h *Handler) run(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run") == "true"
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := h.svc.Run(r.Context(), TriggerManual, &p.UserID, dry)
	switch {
	case errors.Is(err, ErrAlreadyRunning):
		httpx.Error(w, http.StatusConflict, "retention_running", "Penghapusan data sedang berjalan. Coba lagi nanti.")
	case err != nil:
		h.serverError(w, r, err)
	default:
		httpx.JSON(w, http.StatusOK, res)
	}
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("retention request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}
