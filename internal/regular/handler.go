package regular

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/visit"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Handler exposes /v1/regular-guests. Only an owner writes (and only to their own house); guards and
// admins read, because the guard has to pick the guest at the gate.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	owner := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleOwner) }
	reader := func(f http.HandlerFunc) http.Handler {
		return h.authn.Require(f, auth.RoleOwner, auth.RoleGuard, auth.RoleAdmin)
	}
	mux.Handle("POST /v1/regular-guests", owner(h.create))
	mux.Handle("GET /v1/regular-guests", reader(h.list))
	mux.Handle("PATCH /v1/regular-guests/{id}", owner(h.update))
	mux.Handle("DELETE /v1/regular-guests/{id}", owner(h.delete))
}

type createRequest struct {
	GuestName string  `json:"guest_name"`
	Plate     *string `json:"plate"`
	Category  string  `json:"category"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	name := strings.TrimSpace(req.GuestName)
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		fields["guest_name"] = "Nama wajib diisi."
	case n > visit.MaxNameLen:
		fields["guest_name"] = "Nama maksimal " + strconv.Itoa(visit.MaxNameLen) + " karakter."
	}
	in := CreateInput{GuestName: name, Category: req.Category}
	if in.Category == "" {
		in.Category = "tamu"
	}
	if !visit.ValidCategory(in.Category) {
		fields["category"] = "Kategori harus tamu, kurir, ojol, tukang, keluarga, atau lainnya."
	}
	if req.Plate != nil && strings.TrimSpace(*req.Plate) != "" {
		plate, ok := visit.NormalizePlate(*req.Plate)
		if !ok {
			fields["plate"] = "Plat nomor hanya huruf dan angka, maksimal 12 karakter."
		}
		in.Plate = &plate
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	g, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, g)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if v := q.Get("house_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["house_id"] = "house_id tidak valid."
		}
		f.HouseID = &id
	}
	switch v := q.Get("active"); v {
	case "":
	case "true", "false":
		b := v == "true"
		f.Active = &b
	default:
		fields["active"] = "active harus true atau false."
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			fields["limit"] = "limit harus 1 sampai " + strconv.Itoa(maxLimit) + "."
		} else {
			f.Limit = n
		}
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := h.svc.List(r.Context(), p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

type updateRequest struct {
	GuestName *string `json:"guest_name"`
	// Plate: a value sets it, an empty string removes it, leaving it out keeps it.
	Plate    *string `json:"plate"`
	Category *string `json:"category"`
	Active   *bool   `json:"active"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req updateRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.GuestName == nil && req.Plate == nil && req.Category == nil && req.Active == nil {
		httpx.ValidationError(w, map[string]string{"body": "Tidak ada perubahan yang dikirim."})
		return
	}
	fields := map[string]string{}
	in := UpdateInput{Active: req.Active}
	if req.GuestName != nil {
		name := strings.TrimSpace(*req.GuestName)
		switch n := utf8.RuneCountInString(name); {
		case n == 0:
			fields["guest_name"] = "Nama wajib diisi."
		case n > visit.MaxNameLen:
			fields["guest_name"] = "Nama maksimal " + strconv.Itoa(visit.MaxNameLen) + " karakter."
		}
		in.GuestName = &name
	}
	if req.Category != nil {
		if !visit.ValidCategory(*req.Category) {
			fields["category"] = "Kategori harus tamu, kurir, ojol, tukang, keluarga, atau lainnya."
		}
		in.Category = req.Category
	}
	if req.Plate != nil {
		if strings.TrimSpace(*req.Plate) == "" {
			in.ClearPlate = true
		} else {
			plate, ok := visit.NormalizePlate(*req.Plate)
			if !ok {
				fields["plate"] = "Plat nomor hanya huruf dan angka, maksimal 12 karakter."
			}
			in.Plate = &plate
		}
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	g, err := h.svc.Update(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, g)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := h.svc.Delete(r.Context(), p, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "regular_guest_not_found", "Tamu langganan tidak ditemukan.")
	case errors.Is(err, ErrLimitReached):
		httpx.Error(w, http.StatusConflict, "regular_guest_limit", "Terlalu banyak tamu langganan aktif untuk rumah ini. Hapus atau nonaktifkan yang tidak dipakai.")
	case errors.Is(err, ErrNoHouse):
		httpx.Error(w, http.StatusForbidden, "forbidden", "Akun ini belum terhubung ke rumah.")
	case errors.Is(err, httpx.ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("regular guest request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "regular_guest_not_found", "Tamu langganan tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}
