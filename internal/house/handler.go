package house

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Handler exposes the /v1/houses endpoints. Guards and admins may read; only admins write.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	read := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, "guard", "admin") }
	write := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, "admin") }
	mux.Handle("GET /v1/houses", read(h.list))
	mux.Handle("GET /v1/houses/{id}", read(h.get))
	mux.Handle("POST /v1/houses", write(h.create))
	mux.Handle("PATCH /v1/houses/{id}", write(h.update))
}

type createRequest struct {
	Block  string `json:"block"`
	Number string `json:"number"`
}

type updateRequest struct {
	Block  *string `json:"block"`
	Number *string `json:"number"`
	Active *bool   `json:"active"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	// Only admins may see deactivated houses; guards need just the active ones.
	include := p.Role == "admin" && r.URL.Query().Get("include_inactive") == "true"
	houses, err := h.svc.List(r.Context(), include)
	if err != nil {
		serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": houses})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	house, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, house)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	block, number := normalizeBlock(req.Block), normalizeNumber(req.Number)
	if fields := validateFields(&block, &number); len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	house, err := h.svc.Create(r.Context(), p.UserID, block, number)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, house)
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
	in := UpdateInput{Active: req.Active}
	if req.Block != nil {
		b := normalizeBlock(*req.Block)
		in.Block = &b
	}
	if req.Number != nil {
		n := normalizeNumber(*req.Number)
		in.Number = &n
	}
	if in.Block == nil && in.Number == nil && in.Active == nil {
		httpx.ValidationError(w, map[string]string{"body": "Tidak ada perubahan yang dikirim."})
		return
	}
	if fields := validateFields(in.Block, in.Number); len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	house, err := h.svc.Update(r.Context(), p.UserID, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, house)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "house_not_found", "Rumah tidak ditemukan.")
	case errors.Is(err, ErrConflict):
		httpx.Error(w, http.StatusConflict, "house_exists", "Rumah dengan blok dan nomor itu sudah ada.")
	default:
		serverError(w, r, err)
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// A malformed ID can never match a record.
		httpx.Error(w, http.StatusNotFound, "house_not_found", "Rumah tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("house request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}
