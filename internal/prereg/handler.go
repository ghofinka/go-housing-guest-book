package prereg

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
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

// Handler exposes /v1/pre-registrations. Owners create, view and cancel their own house's;
// guards and admins look a code up; admins can also list everything.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	owner := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleOwner) }
	reader := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleOwner, auth.RoleAdmin) }
	lookup := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleGuard, auth.RoleAdmin) }
	mux.Handle("POST /v1/pre-registrations", owner(h.create))
	mux.Handle("GET /v1/pre-registrations", reader(h.list))
	mux.Handle("GET /v1/pre-registrations/{id}", reader(h.get))
	mux.Handle("POST /v1/pre-registrations/{id}/cancel", owner(h.cancel))
	mux.Handle("GET /v1/pre-registrations/by-code/{code}", lookup(h.byCode))
}

type createRequest struct {
	GuestName    string  `json:"guest_name"`
	ExpectedDate string  `json:"expected_date"`
	Plate        *string `json:"plate"`
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
		fields["guest_name"] = "Nama tamu wajib diisi."
	case n > visit.MaxNameLen:
		fields["guest_name"] = "Nama maksimal " + strconv.Itoa(visit.MaxNameLen) + " karakter."
	}

	in := CreateInput{GuestName: name}
	if strings.TrimSpace(req.ExpectedDate) == "" {
		fields["expected_date"] = "Tanggal kedatangan wajib diisi."
	} else if day, ok := httpx.ParseDay(strings.TrimSpace(req.ExpectedDate), "expected_date", fields); ok {
		today := time.Now().In(httpx.WIB)
		start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, httpx.WIB)
		switch {
		case day.Before(start):
			fields["expected_date"] = "Tanggal kedatangan tidak boleh sudah lewat."
		case day.After(start.AddDate(0, 0, MaxDaysAhead)):
			fields["expected_date"] = "Tanggal kedatangan maksimal " + strconv.Itoa(MaxDaysAhead) + " hari ke depan."
		default:
			in.ExpectedDate = day
		}
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
	pr, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, pr)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	pr, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pr)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	pr, err := h.svc.Cancel(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pr)
}

func (h *Handler) byCode(w http.ResponseWriter, r *http.Request) {
	code, ok := visit.NormalizeCode(r.PathValue("code"))
	if !ok {
		// A malformed code can never match a record.
		httpx.Error(w, http.StatusNotFound, "prereg_not_found", "Kode tidak ditemukan.")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	pr, err := h.svc.LookupByCode(r.Context(), p, code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pr)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if f.Status != "" && f.Status != statusActive && f.Status != statusUsed && f.Status != statusCancelled {
		fields["status"] = "Status harus active, used, atau cancelled."
	}
	if v := q.Get("house_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["house_id"] = "house_id tidak valid."
		}
		f.HouseID = &id
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

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "prereg_not_found", "Pre-registrasi tidak ditemukan.")
	case errors.Is(err, ErrLimitReached):
		httpx.Error(w, http.StatusConflict, "prereg_limit", "Terlalu banyak pre-registrasi aktif untuk rumah ini. Batalkan yang tidak dipakai.")
	case errors.Is(err, ErrAlreadyUsed):
		httpx.Error(w, http.StatusConflict, "prereg_used", "Kode ini sudah dipakai, jadi tidak bisa dibatalkan.")
	case errors.Is(err, ErrNoHouse):
		httpx.Error(w, http.StatusForbidden, "forbidden", "Akun ini belum terhubung ke rumah.")
	case errors.Is(err, httpx.ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("pre-registration request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "prereg_not_found", "Pre-registrasi tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}
