package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/server"
)

const (
	adminEmail    = "admin@example.test"
	adminPassword = "Admin Test Pass 1"
)

// newEnv starts a real Postgres, applies the migrations as the owner, and
// returns an API handler backed by the non-owner guestbook_app role.
func newEnv(t *testing.T) (http.Handler, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("guestbook"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres (is Docker running?): %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	ownerURL, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgxpool.New(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)

	files, _ := filepath.Glob("../../migrations/*.up.sql")
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	cfg := owner.Config().ConnConfig.Copy()
	cfg.User, cfg.Password = "guestbook_app", "guestbook_app"
	pc, _ := pgxpool.ParseConfig("")
	pc.ConnConfig = cfg
	app, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)

	tokens := auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute)
	svc, err := auth.NewService(app, tokens, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(server.Deps{DB: app, Routes: []server.Registrar{auth.NewHandler(svc, tokens)}})

	seedUser(t, owner, "Admin Test", adminEmail, adminPassword, "admin", true)
	return h, owner, app
}

func seedUser(t *testing.T, pool *pgxpool.Pool, name, email, password, role string, active bool) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(),
		`INSERT INTO users (name, email, password_hash, role, active) VALUES ($1,$2,$3,$4,$5)`,
		name, email, hash, role, active)
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, h http.Handler, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "192.0.2.1:1234"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type pair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	User         struct {
		Role string `json:"role"`
	} `json:"user"`
}

func login(t *testing.T, h http.Handler) pair {
	t.Helper()
	rec := call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": adminEmail, "password": adminPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body)
	}
	return decode[pair](t, rec)
}

func TestLoginAndMe(t *testing.T) {
	h, _, _ := newEnv(t)
	p := login(t, h)
	if p.TokenType != "Bearer" || p.User.Role != "admin" || p.AccessToken == "" || p.RefreshToken == "" {
		t.Fatalf("unexpected pair: %+v", p)
	}
	rec := call(t, h, "GET", "/v1/auth/me", p.AccessToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me status = %d body=%s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "GET", "/v1/auth/me", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without token = %d, want 401", rec.Code)
	}
}

func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h, owner, _ := newEnv(t)
	seedUser(t, owner, "Gone", "gone@example.test", "Gone Test Pass 1", "guard", false)

	tests := []struct {
		name, email, password string
	}{
		{"wrong password", adminEmail, "nope nope nope"},
		{"unknown email", "nobody@example.test", adminPassword},
		{"inactive user with right password", "gone@example.test", "Gone Test Pass 1"},
	}
	var bodies []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": tt.email, "password": tt.password})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			bodies = append(bodies, rec.Body.String())
		})
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatalf("responses differ: %q vs %q", b, bodies[0])
		}
	}
}

func TestLoginValidation(t *testing.T) {
	h, _, _ := newEnv(t)
	rec := call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": "bad", "password": ""})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	body := decode[struct {
		Error struct {
			Details map[string]string `json:"details"`
		} `json:"error"`
	}](t, rec)
	if body.Error.Details["email"] == "" || body.Error.Details["password"] == "" {
		t.Fatalf("missing field details: %s", rec.Body)
	}
}

func TestLoginRateLimited(t *testing.T) {
	h, _, _ := newEnv(t)
	var last int
	for i := 0; i < 8; i++ {
		last = call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": adminEmail, "password": "wrong"}).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after burst = %d, want 429", last)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	h, _, _ := newEnv(t)
	p1 := login(t, h)
	rec := call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p1.RefreshToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d body=%s", rec.Code, rec.Body)
	}
	p2 := decode[pair](t, rec)
	if p2.RefreshToken == p1.RefreshToken {
		t.Fatal("refresh token was not rotated")
	}
	if rec := call(t, h, "GET", "/v1/auth/me", p2.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("new access token rejected: %d", rec.Code)
	}
}

func TestRefreshTokenReuseRevokesFamily(t *testing.T) {
	h, owner, _ := newEnv(t)
	p1 := login(t, h)
	p2 := decode[pair](t, call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p1.RefreshToken}))

	// Replaying the already-rotated token is rejected...
	if rec := call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p1.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse = %d, want 401", rec.Code)
	}
	// ...and kills the legitimate descendant too.
	if rec := call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p2.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("descendant after reuse = %d, want 401", rec.Code)
	}
	var n int
	if err := owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_logs WHERE action='refresh_reuse_detected'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reuse audit rows = %d, err=%v; want 1", n, err)
	}
}

func TestLogoutInvalidatesRefreshToken(t *testing.T) {
	h, _, _ := newEnv(t)
	p := login(t, h)
	for i := 0; i < 2; i++ { // second call proves idempotency
		if rec := call(t, h, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": p.RefreshToken}); rec.Code != http.StatusNoContent {
			t.Fatalf("logout #%d = %d, want 204", i+1, rec.Code)
		}
	}
	if rec := call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout = %d, want 401", rec.Code)
	}
}

func TestRefreshRejectedAfterUserDeactivated(t *testing.T) {
	h, owner, _ := newEnv(t)
	p := login(t, h)
	if _, err := owner.Exec(context.Background(), `UPDATE users SET active=false WHERE email=$1`, adminEmail); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, h, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": p.RefreshToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh for inactive user = %d, want 401", rec.Code)
	}
	// /me also re-checks the account, so a live access token stops working at once.
	if rec := call(t, h, "GET", "/v1/auth/me", p.AccessToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me for inactive user = %d, want 401", rec.Code)
	}
}

func TestAuthEventsAreAuditedAndLogIsAppendOnly(t *testing.T) {
	h, _, app := newEnv(t)
	call(t, h, "POST", "/v1/auth/login", "", map[string]string{"email": adminEmail, "password": "wrong password"})
	p := login(t, h)
	call(t, h, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": p.RefreshToken})

	rows, err := app.Query(context.Background(), `SELECT action FROM audit_logs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"login_failed", "login", "logout"}
	if len(actions) != len(want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("audit actions = %v, want %v", actions, want)
		}
	}
	for _, stmt := range []string{`UPDATE audit_logs SET action='x'`, `DELETE FROM audit_logs`} {
		if _, err := app.Exec(context.Background(), stmt); err == nil {
			t.Fatalf("app role must not run %q", stmt)
		}
	}
}
