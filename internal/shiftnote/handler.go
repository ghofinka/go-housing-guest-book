package shiftnote

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

// Handler exposes /v1/shift-notes. Guards and admins read and write; only an admin deletes.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	staff := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleGuard, auth.RoleAdmin) }
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("POST /v1/shift-notes", staff(h.create))
	mux.Handle("GET /v1/shift-notes", staff(h.list))
	mux.Handle("DELETE /v1/shift-notes/{id}", admin(h.delete))
}

type createRequest struct {
	Body string `json:"body"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	body := strings.TrimSpace(req.Body)
	switch n := utf8.RuneCountInString(body); {
	case n == 0:
		httpx.ValidationError(w, map[string]string{"body": "Catatan wajib diisi."})
		return
	case n > MaxBodyLen:
		httpx.ValidationError(w, map[string]string{"body": "Catatan maksimal " + strconv.Itoa(MaxBodyLen) + " karakter."})
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	n, err := h.svc.Create(r.Context(), p, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, n)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	from, hasFrom := httpx.ParseDay(q.Get("from"), "from", fields)
	to, hasTo := httpx.ParseDay(q.Get("to"), "to", fields)
	if hasFrom {
		f.From = &from
	}
	if hasTo {
		// "to" is a day and is included: the notes of that day are before the next midnight.
		before := to.AddDate(0, 0, 1)
		f.Before = &before
	}
	if hasFrom && hasTo && to.Before(from) {
		fields["to"] = "Tanggal akhir tidak boleh sebelum tanggal awal."
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

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "shift_note_not_found", "Catatan tidak ditemukan.")
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
		httpx.Error(w, http.StatusNotFound, "shift_note_not_found", "Catatan tidak ditemukan.")
	case errors.Is(err, httpx.ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("shift note request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}
