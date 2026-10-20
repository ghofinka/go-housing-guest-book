// Package e2e holds HTTP-level tests that run the real handlers against a real
// Postgres (testcontainers), connected as the non-owner guestbook_app role.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/house"
	"github.com/ghofinka/go-housing-guest-book/internal/server"
	"github.com/ghofinka/go-housing-guest-book/internal/user"
)

const testPassword = "Test Password 1"

var (
	handler http.Handler
	owner   *pgxpool.Pool // table owner: seeding and assertions only
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup:", err)
		code = 1
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
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
		return 0, fmt.Errorf("start postgres (is Docker running?): %w", err)
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return 0, err
	}
	owner, err = pgxpool.New(ctx, url)
	if err != nil {
		return 0, err
	}
	defer owner.Close()

	files, _ := filepath.Glob("../../migrations/*.up.sql")
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		if _, err := owner.Exec(ctx, string(sql)); err != nil {
			return 0, fmt.Errorf("apply %s: %w", f, err)
		}
	}

	cfg := owner.Config().ConnConfig.Copy()
	cfg.User, cfg.Password = "guestbook_app", "guestbook_app"
	pc, _ := pgxpool.ParseConfig("")
	pc.ConnConfig = cfg
	app, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return 0, err
	}
	defer app.Close()

	tokens := auth.NewTokenIssuer([]byte("0123456789abcdef0123456789abcdef"), 15*time.Minute)
	svc, err := auth.NewService(app, tokens, time.Hour)
	if err != nil {
		return 0, err
	}
	handler = server.New(server.Deps{DB: app, Routes: []server.Registrar{
		auth.NewHandler(svc, tokens),
		house.NewHandler(house.NewService(app), tokens),
		user.NewHandler(user.NewService(app), tokens),
	}})
	return m.Run(), nil
}

// seed inserts an account directly as the table owner.
func seed(t *testing.T, name, email, role string, houseID any) {
	t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(context.Background(),
		`INSERT INTO users (name, email, password_hash, role, house_id) VALUES ($1,$2,$3,$4,$5)`,
		name, email, hash, role, houseID); err != nil {
		t.Fatal(err)
	}
}

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// login returns a token pair for a seeded or created account.
func login(t *testing.T, email string) tokenPair {
	t.Helper()
	rec := do(t, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": testPassword})
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s = %d body=%s", email, rec.Code, rec.Body)
	}
	return decode[tokenPair](t, rec)
}

func do(t *testing.T, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	// Distinct client per call site keeps the login rate limiter out of the way.
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", time.Now().UnixNano()%250+1)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
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

func count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := owner.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
