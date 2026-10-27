// Command retention runs one retention pass (see internal/retention) and exits.
// Use it from Windows Task Scheduler or cron, or to check a run by hand:
//
//	go run ./cmd/retention -dry-run   # report what would be deleted, change nothing
//	go run ./cmd/retention            # do it
//
// It needs DATABASE_URL and PHOTO_DIR (same values as the API). Exit code 0 means
// the run completed with nothing left undone; 1 means an error, or that some photo
// files could not be deleted (they are retried by the next run).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/photo"
	"github.com/ghofinka/go-housing-guest-book/internal/retention"
)

func main() {
	dry := flag.Bool("dry-run", false, "report what would be deleted without changing anything")
	flag.Parse()
	if err := run(*dry); err != nil {
		slog.Error("retention failed", "err", err)
		os.Exit(1)
	}
}

func run(dry bool) error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	photoDir := os.Getenv("PHOTO_DIR")
	if photoDir == "" {
		photoDir = "data/photos"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	store, err := photo.NewLocalStore(photoDir)
	if err != nil {
		return err
	}
	defer store.Close()

	res, err := retention.NewService(pool, store).Run(ctx, retention.TriggerCLI, nil, dry)
	if err != nil {
		return err
	}
	verb := "deleted"
	if dry {
		verb = "would delete"
	}
	fmt.Printf("run %d: %s %d photo file(s); %s personal data of %d record(s); %d file(s) failed\n",
		res.RunID, verb, res.PhotosDeleted, map[bool]string{true: "would anonymize", false: "anonymized"}[dry], res.RecordsAnonymized, res.FilesFailed)
	if res.FilesFailed > 0 {
		return fmt.Errorf("%d photo file(s) could not be deleted", res.FilesFailed)
	}
	return nil
}
