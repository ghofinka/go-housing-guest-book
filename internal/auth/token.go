package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Role values match the users.role CHECK constraint.
const (
	RoleGuard = "guard"
	RoleOwner = "owner"
	RoleAdmin = "admin"
)

// Principal is the authenticated caller. Later features use it to filter by
// house and to set the RLS session variables (app.role, app.house_id).
type Principal struct {
	UserID  uuid.UUID
	Role    string
	HouseID *uuid.UUID // set for owners only
}

type claims struct {
	Role    string `json:"role"`
	HouseID string `json:"house_id,omitempty"`
	jwt.RegisteredClaims
}

// TokenIssuer signs and verifies access tokens (HS256).
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func NewTokenIssuer(secret []byte, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, ttl: ttl, now: time.Now}
}

// TTL is the access token lifetime.
func (t *TokenIssuer) TTL() time.Duration { return t.ttl }

// Issue returns a signed access token for p.
func (t *TokenIssuer) Issue(p Principal) (string, error) {
	now := t.now()
	c := claims{
		Role: p.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
		},
	}
	if p.HouseID != nil {
		c.HouseID = p.HouseID.String()
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
	if err != nil {
		return "", fmt.Errorf("sign access token: %w", err)
	}
	return s, nil
}

// ErrInvalidToken covers every verification failure (expired, tampered, wrong alg...).
var ErrInvalidToken = errors.New("invalid token")

// Parse verifies the signature (HS256 only), expiry and claims.
func (t *TokenIssuer) Parse(token string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) {
		return t.secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(), jwt.WithTimeFunc(t.now))
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	uid, err := uuid.Parse(c.Subject)
	if err != nil {
		return Principal{}, ErrInvalidToken
	}
	switch c.Role {
	case RoleGuard, RoleOwner, RoleAdmin:
	default:
		return Principal{}, ErrInvalidToken
	}
	p := Principal{UserID: uid, Role: c.Role}
	if c.HouseID != "" {
		hid, err := uuid.Parse(c.HouseID)
		if err != nil {
			return Principal{}, ErrInvalidToken
		}
		p.HouseID = &hid
	}
	if (p.Role == RoleOwner) != (p.HouseID != nil) {
		return Principal{}, ErrInvalidToken
	}
	return p, nil
}

// newRefreshToken returns an opaque random token and its storable hash.
func newRefreshToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashRefreshToken(token), nil
}

func hashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
