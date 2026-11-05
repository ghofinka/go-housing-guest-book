package watchlist

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
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Handler exposes /v1/watchlist. Admin only: a guard learns of a match at check-in and nowhere else.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("POST /v1/watchlist", admin(h.create))
	mux.Handle("GET /v1/watchlist", admin(h.list))
	mux.Handle("PATCH /v1/watchlist/{id}", admin(h.update))
	mux.Handle("DELETE /v1/watchlist/{id}", admin(h.delete))
}

type createRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	Note  string `json:"note"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	var value string
	switch req.Kind {
	case KindPlate:
		v, ok := NormalizePlate(req.Value)
		if !ok {
			fields["value"] = "Plat nomor hanya huruf dan angka, 2 sampai " + strconv.Itoa(maxPlateLen) + " karakter."
		}
		value = v
	case KindName:
		v, ok := NormalizeName(req.Value)
		if !ok {
			fields["value"] = "Nama harus 2 sampai " + strconv.Itoa(maxNameLen) + " karakter."
		}
		value = v
	default:
		fields["kind"] = "Jenis harus plate atau name."
	}
	note := strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(note) > maxNoteLen {
		fields["note"] = "Catatan maksimal " + strconv.Itoa(maxNoteLen) + " karakter."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	e, err := h.svc.Create(r.Context(), actor, req.Kind, value, note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, e)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Kind: q.Get("kind"), Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if f.Kind != "" && f.Kind != KindPlate && f.Kind != KindName {
		fields["kind"] = "Jenis harus plate atau name."
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
	actor, _ := auth.PrincipalFrom(r.Context())
	res, err := h.svc.List(r.Context(), actor, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

type updateRequest struct {
	Note   *string `json:"note"`
	Active *bool   `json:"active"`
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
	if req.Note == nil && req.Active == nil {
		httpx.ValidationError(w, map[string]string{"body": "Tidak ada perubahan yang dikirim."})
		return
	}
	if req.Note != nil {
		t := strings.TrimSpace(*req.Note)
		if utf8.RuneCountInString(t) > maxNoteLen {
			httpx.ValidationError(w, map[string]string{"note": "Catatan maksimal " + strconv.Itoa(maxNoteLen) + " karakter."})
			return
		}
		req.Note = &t
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	e, err := h.svc.Update(r.Context(), actor, id, req.Note, req.Active)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	if err := h.svc.Delete(r.Context(), actor, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "watchlist_not_found", "Data daftar hitam tidak ditemukan.")
	case errors.Is(err, ErrExists):
		httpx.Error(w, http.StatusConflict, "watchlist_exists", "Plat atau nama itu sudah ada di daftar hitam yang aktif.")
	case errors.Is(err, httpx.ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("watchlist request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "watchlist_not_found", "Data daftar hitam tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}
