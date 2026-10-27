package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/audit"
	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/config"
	"github.com/ghofinka/go-housing-guest-book/internal/house"
	"github.com/ghofinka/go-housing-guest-book/internal/photo"
	"github.com/ghofinka/go-housing-guest-book/internal/privacy"
	"github.com/ghofinka/go-housing-guest-book/internal/retention"
	"github.com/ghofinka/go-housing-guest-book/internal/server"
	"github.com/ghofinka/go-housing-guest-book/internal/user"
	"github.com/ghofinka/go-housing-guest-book/internal/visit"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	photos, err := photo.NewLocalStore(cfg.PhotoDir)
	if err != nil {
		return err
	}
	defer photos.Close()

	tokens := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL)
	authSvc, err := auth.NewService(pool, tokens, cfg.RefreshTokenTTL)
	if err != nil {
		return err
	}
	authn := auth.NewAuthenticator(tokens, authSvc)

	retentionSvc := retention.NewService(pool, photos)
	if cfg.RetentionEnabled {
		hour, minute, err := retention.ParseRunAt(cfg.RetentionRunAt)
		if err != nil {
			return fmt.Errorf("RETENTION_RUN_AT: %w", err)
		}
		go retention.NewScheduler(retentionSvc, hour, minute).Run(ctx)
	} else {
		slog.Warn("retention job disabled (RETENTION_ENABLED=false): photos and personal data will not expire unless cmd/retention is scheduled")
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: server.New(server.Deps{
			DB:         pool,
			CORSOrigin: cfg.CORSOrigin,
			Routes: []server.Registrar{
				auth.NewHandler(authSvc, authn),
				house.NewHandler(house.NewService(pool), authn),
				user.NewHandler(user.NewService(pool), authn),
				visit.NewHandler(visit.NewService(pool, photos), authn),
				audit.NewHandler(audit.NewService(pool), authn),
				retention.NewHandler(retentionSvc, authn),
				privacy.NewHandler(privacy.NewService(pool), authn),
			},
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
}
