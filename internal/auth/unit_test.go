package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, password string
		want           bool
	}{
		{"correct password", "correct horse battery", true},
		{"wrong password", "wrong horse battery", false},
		{"empty password", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := VerifyPassword(tt.password, hash)
			if err != nil || got != tt.want {
				t.Fatalf("VerifyPassword = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
	if _, err := VerifyPassword("x", "not-a-hash"); err == nil {
		t.Fatal("malformed hash should error")
	}
}

func TestTokenParse(t *testing.T) {
	house := uuid.New()
	issuer := NewTokenIssuer(testSecret, time.Minute)
	owner := Principal{UserID: uuid.New(), Role: RoleOwner, HouseID: &house}
	good, _ := issuer.Issue(owner)

	expired := NewTokenIssuer(testSecret, time.Minute)
	expired.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expiredTok, _ := expired.Issue(owner)

	otherKey, _ := NewTokenIssuer([]byte("another-secret-another-secret-123"), time.Minute).Issue(owner)

	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims{
		Role: RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.NewString(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)

	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, claims{
		Role: RoleAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.NewString(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(testSecret)

	ownerNoHouse, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Role: RoleOwner,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.NewString(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(testSecret)

	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{"valid owner token", good, true},
		{"expired token", expiredTok, false},
		{"signed with another key", otherKey, false},
		{"alg none", none, false},
		{"alg HS512", hs512, false},
		{"owner without house", ownerNoHouse, false},
		{"tampered payload", good[:len(good)-3] + "abc", false},
		{"garbage", "not.a.jwt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := issuer.Parse(tt.token)
			if (err == nil) != tt.ok {
				t.Fatalf("Parse err = %v, want ok=%v", err, tt.ok)
			}
			if tt.ok && (p.UserID != owner.UserID || p.Role != RoleOwner || *p.HouseID != house) {
				t.Fatalf("principal = %+v", p)
			}
		})
	}
}

type fakeLoader struct {
	p   Principal
	err error
}

func (f fakeLoader) LoadPrincipal(context.Context, uuid.UUID) (Principal, error) { return f.p, f.err }

func TestAuthenticatorRequireRole(t *testing.T) {
	issuer := NewTokenIssuer(testSecret, time.Minute)
	id := uuid.New()
	guardTok, _ := issuer.Issue(Principal{UserID: id, Role: RoleGuard})
	adminTok, _ := issuer.Issue(Principal{UserID: id, Role: RoleAdmin})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	tests := []struct {
		name   string
		loader fakeLoader
		header string
		want   int
	}{
		{"no header", fakeLoader{p: Principal{UserID: id, Role: RoleAdmin}}, "", http.StatusUnauthorized},
		{"wrong scheme", fakeLoader{p: Principal{UserID: id, Role: RoleAdmin}}, "Basic " + guardTok, http.StatusUnauthorized},
		{"bad token", fakeLoader{p: Principal{UserID: id, Role: RoleAdmin}}, "Bearer nope", http.StatusUnauthorized},
		{"guard on admin route", fakeLoader{p: Principal{UserID: id, Role: RoleGuard}}, "Bearer " + guardTok, http.StatusForbidden},
		{"admin on admin route", fakeLoader{p: Principal{UserID: id, Role: RoleAdmin}}, "Bearer " + adminTok, http.StatusOK},
		// The token's role claim is ignored: the database says what the account is now.
		{"token says admin, account is now guard", fakeLoader{p: Principal{UserID: id, Role: RoleGuard}}, "Bearer " + adminTok, http.StatusForbidden},
		{"token says guard, account is now admin", fakeLoader{p: Principal{UserID: id, Role: RoleAdmin}}, "Bearer " + guardTok, http.StatusOK},
		{"valid token, account deactivated", fakeLoader{err: ErrInvalidCredentials}, "Bearer " + adminTok, http.StatusUnauthorized},
		{"valid token, lookup fails", fakeLoader{err: errors.New("db down")}, "Bearer " + adminTok, http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewAuthenticator(issuer, tt.loader).Require(ok, RoleAdmin)
			req := httptest.NewRequest("GET", "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
func TestLoginLimiterBlocksAfterBurst(t *testing.T) {
	l := newLoginLimiter(3, time.Hour)
	for i := 0; i < 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.Allow("k") {
		t.Fatal("4th attempt should be blocked")
	}
	if !l.Allow("other") {
		t.Fatal("a different key has its own budget")
	}
}
