// Command seed creates (or resets the password of) the first admin account.
// Credentials come from the environment so none are committed to the repo:
//
//	ADMIN_EMAIL, ADMIN_PASSWORD (min 10 chars), ADMIN_NAME (optional)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("SEED_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		return errors.New("SEED_DATABASE_URL or DATABASE_URL is required")
	}
	email := strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL")))
	if _, err := mail.ParseAddress(email); err != nil {
		return errors.New("ADMIN_EMAIL must be a valid email")
	}
	password := os.Getenv("ADMIN_PASSWORD")
	if len(password) < auth.MinPasswordLength {
		return fmt.Errorf("ADMIN_PASSWORD must be at least %d characters", auth.MinPasswordLength)
	}
	name := strings.TrimSpace(os.Getenv("ADMIN_NAME"))
	if name == "" {
		name = "Admin"
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	id, err := db.New(pool).UpsertAdminUser(ctx, db.UpsertAdminUserParams{
		Name: name, Email: email, PasswordHash: hash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("that email already belongs to a non-admin account")
	}
	if err != nil {
		return fmt.Errorf("upsert admin: %w", err)
	}
	slog.Info("admin account ready", "user_id", id)
	return nil
}
