package retention

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// ParseRunAt reads a daily run time such as "02:00" (WIB).
func ParseRunAt(s string) (hour, minute int, err error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hour, err1 := strconv.Atoi(h)
	minute, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("run time %q must look like HH:MM, for example 02:00", s)
	}
	return hour, minute, nil
}

// NextRun returns the next hh:mm in WIB strictly after now. WIB has no DST, so a day is always 24 hours.
func NextRun(now time.Time, hour, minute int) time.Time {
	n := now.In(httpx.WIB)
	t := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, httpx.WIB)
	if !t.After(n) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// Scheduler runs the retention job once a day inside the API process. The advisory
// lock in Service.Run keeps several API instances from running it at the same time.
type Scheduler struct {
	svc          *Service
	hour, minute int
	now          func() time.Time
}

func NewScheduler(svc *Service, hour, minute int) *Scheduler {
	return &Scheduler{svc: svc, hour: hour, minute: minute, now: time.Now}
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next := NextRun(s.now(), s.hour, s.minute)
		slog.Info("retention scheduled", "next_run", next.Format(time.RFC3339))
		select {
		case <-ctx.Done():
			return
		case <-time.After(next.Sub(s.now())):
		}
		if _, err := s.svc.Run(ctx, TriggerSchedule, nil, false); err != nil {
			// Already logged and recorded by Run; ErrAlreadyRunning just means another instance has it.
			slog.Warn("scheduled retention run did not complete", "err", err)
		}
	}
}
