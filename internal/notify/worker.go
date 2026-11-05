package notify

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

const (
	// MaxAttempts is how many times one notification is tried before it is marked failed.
	MaxAttempts = 5
	// retention of finished rows; the rows hold ids and times only.
	finishedRowAge = 30 * 24 * time.Hour

	defaultInterval = 15 * time.Second
	defaultBatch    = 20
	cleanupEvery    = time.Hour
	baseBackoff     = time.Minute
)

// Stats reports what one Tick did.
type Stats struct {
	Sent, Retried, Failed, Skipped int
}

// Worker sends queued notifications. It is safe to run in several API instances at once: rows are
// claimed with FOR UPDATE SKIP LOCKED, and the send happens outside any transaction.
type Worker struct {
	pool     *pgxpool.Pool
	sender   Sender
	interval time.Duration
	batch    int32
	now      func() time.Time

	lastCleanup time.Time
}

func NewWorker(pool *pgxpool.Pool, sender Sender) *Worker {
	return &Worker{pool: pool, sender: sender, interval: defaultInterval, batch: defaultBatch, now: time.Now}
}

// WithBatchSize sets how many notifications one Tick claims (default 20). A small batch lets several
// workers share a backlog, which is what the concurrency tests rely on.
func (w *Worker) WithBatchSize(n int) *Worker {
	if n > 0 {
		w.batch = int32(n)
	}
	return w
}

// Run ticks until ctx is cancelled. A send that is already under way is finished and recorded first
// (the caller waits for Run to return before closing the pool).
func (w *Worker) Run(ctx context.Context) {
	slog.Info("notification worker started", "interval", w.interval.String())
	defer slog.Info("notification worker stopped")
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		if s := w.Tick(ctx); s != (Stats{}) {
			// Counts only: never an address or any message content.
			slog.Info("notifications processed", "sent", s.Sent, "retried", s.Retried, "failed", s.Failed, "skipped", s.Skipped)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick claims the due notifications, sends them and records each outcome. Once an hour it also
// deletes finished rows older than 30 days. It never returns an error: a database problem is logged
// and the next tick tries again.
func (w *Worker) Tick(ctx context.Context) Stats {
	var stats Stats
	var rows []db.ClaimNotificationsRow
	// The claim joins visits, which is under row-level security, so it runs as an admin.
	err := db.InTx(ctx, w.pool, auth.RoleAdmin, nil, func(q *db.Queries) error {
		var err error
		rows, err = q.ClaimNotifications(ctx, w.batch)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("notifications: claim", "err", err)
		}
		return stats
	}

	// The outcome of a send is recorded even if shutdown began meanwhile, or it would be sent twice.
	rec := context.WithoutCancel(ctx)
	q := db.New(w.pool)
	for _, r := range rows {
		switch w.deliver(ctx, rec, q, r) {
		case outcomeSent:
			stats.Sent++
		case outcomeRetry:
			stats.Retried++
		case outcomeFailed:
			stats.Failed++
		case outcomeSkipped:
			stats.Skipped++
		}
	}

	if now := w.now(); now.Sub(w.lastCleanup) >= cleanupEvery {
		w.lastCleanup = now
		if n, err := q.DeleteOldNotifications(rec, now.Add(-finishedRowAge)); err != nil {
			slog.Error("notifications: cleanup", "err", err)
		} else if n > 0 {
			slog.Info("notifications: old rows deleted", "count", n)
		}
	}
	return stats
}

type outcome int

const (
	outcomeSent outcome = iota
	outcomeRetry
	outcomeFailed
	outcomeSkipped
)

func (w *Worker) deliver(ctx, rec context.Context, q *db.Queries, r db.ClaimNotificationsRow) outcome {
	save := func(err error, what string) {
		if err != nil {
			slog.Error("notifications: record outcome", "id", r.ID, "outcome", what, "err", err)
		}
	}
	if !r.UserActive {
		save(q.MarkNotificationSkipped(rec, db.MarkNotificationSkippedParams{ID: r.ID, LastError: text("recipient is inactive")}), "skipped")
		return outcomeSkipped
	}

	err := w.sender.Send(ctx, arrivalMessage(r.Email, r.Category, r.CheckinAt))
	if err == nil {
		save(q.MarkNotificationSent(rec, r.ID), "sent")
		return outcomeSent
	}

	reason := errorSummary(err)
	slog.Warn("notifications: send failed", "id", r.ID, "attempt", r.Attempts, "reason", reason)
	if int(r.Attempts) >= MaxAttempts {
		save(q.MarkNotificationFailed(rec, db.MarkNotificationFailedParams{ID: r.ID, LastError: text(reason)}), "failed")
		return outcomeFailed
	}
	save(q.RetryNotificationLater(rec, db.RetryNotificationLaterParams{
		ID: r.ID, NextAttemptAt: w.now().Add(backoff(int(r.Attempts))), LastError: text(reason),
	}), "retry")
	return outcomeRetry
}

// backoff is the wait before the next try: 1, 2, 4, 8 minutes after attempts 1 to 4.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	return baseBackoff << (attempts - 1)
}

// errorSummary reduces a send error to a short reason that is safe to store and log. The error text
// itself can contain the server's reply, which may quote the recipient's address.
func errorSummary(err error) string {
	var perr *textproto.Error
	if errors.As(err, &perr) {
		return "smtp reply " + strconv.Itoa(perr.Code)
	}
	var nerr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &nerr) && nerr.Timeout():
		return "network timeout"
	case errors.As(err, &nerr):
		return "network error"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "send error"
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
