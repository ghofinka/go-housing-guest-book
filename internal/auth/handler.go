package auth

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Handler exposes the /v1/auth endpoints.
type Handler struct {
	svc     *Service
	tokens  *TokenIssuer
	limiter *loginLimiter
}

func NewHandler(svc *Service, tokens *TokenIssuer) *Handler {
	return &Handler{svc: svc, tokens: tokens, limiter: newLoginLimiter(5, 12*time.Second)}
}

// Register mounts the auth routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /v1/auth/logout", h.logout)
	mux.Handle("GET /v1/auth/me", Authenticate(h.tokens, http.HandlerFunc(h.me)))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	fields := map[string]string{}
	if _, err := mail.ParseAddress(email); err != nil {
		fields["email"] = "Email tidak valid."
	}
	if req.Password == "" {
		fields["password"] = "Password wajib diisi."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	if !h.limiter.Allow(clientIP(r) + "|" + email) {
		w.Header().Set("Retry-After", "60")
		httpx.Error(w, http.StatusTooManyRequests, "too_many_attempts", "Terlalu banyak percobaan. Coba lagi sebentar.")
		return
	}
	pair, err := h.svc.Login(r.Context(), email, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			httpx.Error(w, http.StatusUnauthorized, "invalid_credentials", "Email atau password salah.")
			return
		}
		serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		httpx.ValidationError(w, map[string]string{"refresh_token": "Refresh token wajib diisi."})
		return
	}
	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrInvalidRefresh) {
			httpx.Error(w, http.StatusUnauthorized, "invalid_refresh_token", "Sesi berakhir. Silakan login kembali.")
			return
		}
		serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, pair)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	if req.RefreshToken == "" {
		httpx.ValidationError(w, map[string]string{"refresh_token": "Refresh token wajib diisi."})
		return
	}
	if err := h.svc.Logout(r.Context(), req.RefreshToken); err != nil {
		serverError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	info, err := h.svc.Me(r.Context(), p)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			unauthorized(w)
			return
		}
		serverError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, info)
}

func serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("auth request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
