package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

type principalKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the authenticated caller, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// PrincipalLoader returns the caller's current account state. It must return
// ErrInvalidCredentials for an unknown or deactivated account.
type PrincipalLoader interface {
	LoadPrincipal(ctx context.Context, userID uuid.UUID) (Principal, error)
}

// Authenticator turns a Bearer access token into a Principal.
//
// The token only proves who the caller is. Role, house and active status are read
// from the database on every request, never taken from the token's claims, so
// deactivating an account, or moving an owner to another house, takes effect
// immediately instead of when the access token expires.
type Authenticator struct {
	tokens *TokenIssuer
	users  PrincipalLoader
}

func NewAuthenticator(tokens *TokenIssuer, users PrincipalLoader) *Authenticator {
	return &Authenticator{tokens: tokens, users: users}
}

// Authenticate requires a valid Bearer access token for an active account and
// stores the caller's current Principal in the context.
func (a *Authenticator) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			unauthorized(w)
			return
		}
		claimed, err := a.tokens.Parse(strings.TrimSpace(token))
		if err != nil {
			unauthorized(w)
			return
		}
		p, err := a.users.LoadPrincipal(r.Context(), claimed.UserID)
		if errors.Is(err, ErrInvalidCredentials) {
			unauthorized(w)
			return
		}
		if err != nil {
			slog.Error("load principal", "request_id", httpx.RequestID(r.Context()), "err", err)
			httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// Require is Authenticate followed by RequireRole(roles...).
func (a *Authenticator) Require(next http.Handler, roles ...string) http.Handler {
	return a.Authenticate(RequireRole(next, roles...))
}

// RequireRole allows only the listed roles. It must run after Authenticate.
func RequireRole(next http.Handler, roles ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			unauthorized(w)
			return
		}
		if !slices.Contains(roles, p.Role) {
			httpx.Error(w, http.StatusForbidden, "forbidden", "Anda tidak punya akses untuk ini.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="guestbook"`)
	httpx.Error(w, http.StatusUnauthorized, "unauthorized", "Silakan login terlebih dahulu.")
}
