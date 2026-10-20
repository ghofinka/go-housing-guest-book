package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/config"
	"github.com/ghofinka/go-housing-guest-book/internal/house"
	"github.com/ghofinka/go-housing-guest-book/internal/server"
	"github.com/ghofinka/go-housing-guest-book/internal/user"
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

	tokens := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTokenTTL)
	authSvc, err := auth.NewService(pool, tokens, cfg.RefreshTokenTTL)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: server.New(server.Deps{
			DB:         pool,
			CORSOrigin: cfg.CORSOrigin,
			Routes: []server.Registrar{
				auth.NewHandler(authSvc, tokens),
				house.NewHandler(house.NewService(pool), tokens),
				user.NewHandler(user.NewService(pool), tokens),
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
