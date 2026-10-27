package user

import (
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

const (
	maxNameLen     = 100
	maxContactLen  = 50
	maxPasswordLen = 128
	defaultLimit   = 50
	maxLimit       = 200
)

// Handler exposes the admin-only /v1/users endpoints.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, "admin") }
	mux.Handle("GET /v1/users", admin(h.list))
	mux.Handle("GET /v1/users/{id}", admin(h.get))
	mux.Handle("POST /v1/users", admin(h.create))
	mux.Handle("PATCH /v1/users/{id}", admin(h.update))
}

type createRequest struct {
	Name     string     `json:"name"`
	Email    string     `json:"email"`
	Password string     `json:"password"`
	Role     string     `json:"role"`
	HouseID  *uuid.UUID `json:"house_id"`
	Contact  *string    `json:"contact"`
}

type updateRequest struct {
	Name     *string    `json:"name"`
	Contact  *string    `json:"contact"`
	HouseID  *uuid.UUID `json:"house_id"`
	Active   *bool      `json:"active"`
	Password *string    `json:"password"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Role: q.Get("role"), Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if f.Role != "" && !validRoles[f.Role] {
		fields["role"] = "Peran harus guard, owner, atau admin."
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
	res, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	u, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, u)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	in := CreateInput{
		Name: strings.TrimSpace(req.Name), Email: strings.ToLower(strings.TrimSpace(req.Email)),
		Password: req.Password, Role: req.Role, HouseID: req.HouseID, Contact: trimPtr(req.Contact),
	}
	fields := validateName(in.Name, true)
	if _, err := mail.ParseAddress(in.Email); err != nil {
		fields["email"] = "Email tidak valid."
	}
	if msg := validatePassword(in.Password); msg != "" {
		fields["password"] = msg
	}
	if !validRoles[in.Role] {
		fields["role"] = "Peran harus guard, owner, atau admin."
	}
	if in.Role == "owner" && in.HouseID == nil {
		fields["house_id"] = "Pemilik rumah wajib punya rumah."
	}
	if in.Role != "owner" && in.HouseID != nil {
		fields["house_id"] = "Hanya pemilik rumah yang terhubung ke rumah."
	}
	addContactError(fields, in.Contact)
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	u, err := h.svc.Create(r.Context(), p.UserID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, u)
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
	in := UpdateInput{Contact: trimPtr(req.Contact), HouseID: req.HouseID, Active: req.Active, Password: req.Password}
	fields := map[string]string{}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		in.Name = &n
		fields = validateName(n, true)
	}
	if in.Password != nil {
		if msg := validatePassword(*in.Password); msg != "" {
			fields["password"] = msg
		}
	}
	addContactError(fields, in.Contact)
	if in.Name == nil && in.Contact == nil && in.HouseID == nil && in.Active == nil && in.Password == nil {
		fields["body"] = "Tidak ada perubahan yang dikirim."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	u, err := h.svc.Update(r.Context(), p.UserID, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, u)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "user_not_found", "Akun tidak ditemukan.")
	case errors.Is(err, ErrEmailTaken):
		httpx.Error(w, http.StatusConflict, "email_taken", "Email sudah dipakai akun lain.")
	case errors.Is(err, ErrHouseInvalid):
		httpx.ValidationError(w, map[string]string{"house_id": "Rumah tidak ditemukan, tidak aktif, atau akun ini bukan pemilik rumah."})
	case errors.Is(err, ErrSelfDeactivation):
		httpx.Error(w, http.StatusConflict, "cannot_deactivate_self", "Anda tidak bisa menonaktifkan akun sendiri.")
	case errors.Is(err, ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("user request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

func validateName(name string, required bool) map[string]string {
	fields := map[string]string{}
	switch n := utf8.RuneCountInString(name); {
	case required && n == 0:
		fields["name"] = "Nama wajib diisi."
	case n > maxNameLen:
		fields["name"] = "Nama maksimal " + strconv.Itoa(maxNameLen) + " karakter."
	}
	return fields
}

func validatePassword(p string) string {
	switch n := utf8.RuneCountInString(p); {
	case n < auth.MinPasswordLength:
		return "Password minimal " + strconv.Itoa(auth.MinPasswordLength) + " karakter."
	case n > maxPasswordLen:
		return "Password maksimal " + strconv.Itoa(maxPasswordLen) + " karakter."
	}
	return ""
}

func addContactError(fields map[string]string, contact *string) {
	if contact != nil && utf8.RuneCountInString(*contact) > maxContactLen {
		fields["contact"] = "Kontak maksimal " + strconv.Itoa(maxContactLen) + " karakter."
	}
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "user_not_found", "Akun tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}
