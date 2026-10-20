package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"

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

// Authenticate requires a valid Bearer access token and stores the Principal in the context.
func Authenticate(tokens *TokenIssuer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
			unauthorized(w)
			return
		}
		p, err := tokens.Parse(strings.TrimSpace(token))
		if err != nil {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
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
