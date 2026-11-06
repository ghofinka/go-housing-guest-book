// Package stats summarises the visit log for a period (PRD C4): visits per day and per hour, the busiest
// hour, the houses with the most visits, how long guests stay, and who is still inside. It returns counts
// only, never a name or plate, so it is safe to show to anyone who is logged in. What each role gets is scoped:
// an admin and a guard see every house, an owner sees only their own.
package stats

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

const (
	// MaxPeriodDays is the longest period one request may cover.
	MaxPeriodDays = 93
	// DefaultDays is the period used when from and to are left out: today and the six days before it.
	DefaultDays = 7
	// TopHouseCount is how many houses the "most visits" list holds.
	TopHouseCount = 10
)

// categories is the fixed list of visit categories, in the order the answer reports them.
var categories = []string{"tamu", "kurir", "ojol", "tukang", "keluarga", "lainnya"}

// Params selects the period and, optionally, one house. From and To are calendar days in WIB (midnight),
// both included.
type Params struct {
	From, To time.Time
	HouseID  *uuid.UUID
}

type Period struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type CategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type DayCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type HourCount struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

type HouseRef struct {
	ID     uuid.UUID `json:"id"`
	Block  string    `json:"block"`
	Number string    `json:"number"`
}

type HouseCount struct {
	House HouseRef `json:"house"`
	Count int      `json:"count"`
}

// Stats is the answer. Every list is complete: a day or an hour with no visits is there with a count of 0, so a
// chart needs no gap-filling.
type Stats struct {
	Period  Period     `json:"period"`
	HouseID *uuid.UUID `json:"house_id"`
	Total   int        `json:"total"`
	// StillInside and Overstay count visits of the period that have no check-out yet, and of those the ones past
	// the long-stay limit (S7).
	StillInside int `json:"still_inside"`
	Overstay    int `json:"overstay"`
	// AverageMinutesInside is over the guests who have left (one decimal); null when nobody has.
	AverageMinutesInside *float64        `json:"average_minutes_inside"`
	ByCategory           []CategoryCount `json:"by_category"`
	ByDay                []DayCount      `json:"by_day"`
	ByHour               []HourCount     `json:"by_hour"`
	// BusiestHour is the hour of the day (WIB) with the most check-ins, the earliest on a tie; null for no visits.
	BusiestHour *HourCount `json:"busiest_hour"`
	// TopHouses is for admins and guards only. An owner has no one to compare with, so it is left out of their
	// answer altogether (the key is absent), not sent empty.
	TopHouses *[]HouseCount `json:"top_houses,omitempty"`
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, now: time.Now} }

// Get computes the statistics for the caller. All the queries run in one repeatable-read transaction, so the
// total, the days and the hours describe the same set of visits even while guests keep arriving.
func (s *Service) Get(ctx context.Context, p auth.Principal, pr Params) (Stats, error) {
	staff := p.Role != auth.RoleOwner
	out := empty(pr, staff)

	if p.Role == auth.RoleOwner {
		// An owner sees only their own house. Asking for another one is not an error (that would confirm it
		// exists): it simply matches nothing, like the visit list.
		if p.HouseID == nil || (pr.HouseID != nil && *pr.HouseID != *p.HouseID) {
			return out, nil
		}
		pr.HouseID = p.HouseID
		out.HouseID = p.HouseID
	}

	// The period ends no later than now, so a window that reaches into the future is fine.
	from, before := pr.From, pr.To.AddDate(0, 0, 1)
	if now := s.now(); before.After(now) {
		before = now
	}
	if !from.Before(before) {
		return out, nil
	}

	var (
		totals db.StatsTotalsRow
		byCat  []db.StatsByCategoryRow
		byDay  []db.StatsByDayRow
		byHour []db.StatsByHourRow
		top    []db.StatsTopHousesRow
	)
	err := db.InReadTx(ctx, s.pool, p.Role, p.HouseID, func(q *db.Queries) error {
		var err error
		if totals, err = q.StatsTotals(ctx, db.StatsTotalsParams{FromAt: from, BeforeAt: before, HouseID: uuidOrNull(pr.HouseID)}); err != nil {
			return fmt.Errorf("totals: %w", err)
		}
		if byCat, err = q.StatsByCategory(ctx, db.StatsByCategoryParams{FromAt: from, BeforeAt: before, HouseID: uuidOrNull(pr.HouseID)}); err != nil {
			return fmt.Errorf("by category: %w", err)
		}
		if byDay, err = q.StatsByDay(ctx, db.StatsByDayParams{FromAt: from, BeforeAt: before, HouseID: uuidOrNull(pr.HouseID)}); err != nil {
			return fmt.Errorf("by day: %w", err)
		}
		if byHour, err = q.StatsByHour(ctx, db.StatsByHourParams{FromAt: from, BeforeAt: before, HouseID: uuidOrNull(pr.HouseID)}); err != nil {
			return fmt.Errorf("by hour: %w", err)
		}
		if staff {
			if top, err = q.StatsTopHouses(ctx, db.StatsTopHousesParams{
				FromAt: from, BeforeAt: before, HouseID: uuidOrNull(pr.HouseID), TopN: TopHouseCount,
			}); err != nil {
				return fmt.Errorf("top houses: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return Stats{}, fmt.Errorf("compute statistics: %w", err)
	}

	out.Total, out.StillInside, out.Overstay = int(totals.Total), int(totals.StillInside), int(totals.Overstay)
	if totals.Exited > 0 {
		v := math.Round(totals.AvgMinutesInside*10) / 10
		out.AverageMinutesInside = &v
	}
	catCount := map[string]int{}
	for _, r := range byCat {
		catCount[r.Category] = int(r.Total)
	}
	for i := range out.ByCategory {
		out.ByCategory[i].Count = catCount[out.ByCategory[i].Category]
	}
	dayCount := map[string]int{}
	for _, r := range byDay {
		dayCount[r.Day.Time.Format(time.DateOnly)] = int(r.Total)
	}
	for i := range out.ByDay {
		out.ByDay[i].Count = dayCount[out.ByDay[i].Date]
	}
	for _, r := range byHour {
		if r.Hour >= 0 && int(r.Hour) < len(out.ByHour) {
			out.ByHour[r.Hour].Count = int(r.Total)
		}
	}
	for _, h := range out.ByHour {
		if h.Count > 0 && (out.BusiestHour == nil || h.Count > out.BusiestHour.Count) {
			best := h
			out.BusiestHour = &best
		}
	}
	if staff {
		houses := make([]HouseCount, 0, len(top))
		for _, r := range top {
			houses = append(houses, HouseCount{House: HouseRef{ID: r.ID, Block: r.Block, Number: r.Number}, Count: int(r.Total)})
		}
		out.TopHouses = &houses
	}
	return out, nil
}

// empty is the answer for no visits: every day, hour and category present with a count of 0.
func empty(pr Params, staff bool) Stats {
	out := Stats{
		Period:     Period{From: pr.From.Format(time.DateOnly), To: pr.To.Format(time.DateOnly)},
		HouseID:    pr.HouseID,
		ByCategory: make([]CategoryCount, len(categories)),
		ByHour:     make([]HourCount, 24),
	}
	for i, c := range categories {
		out.ByCategory[i] = CategoryCount{Category: c}
	}
	for h := range out.ByHour {
		out.ByHour[h] = HourCount{Hour: h}
	}
	for d := pr.From; !d.After(pr.To); d = d.AddDate(0, 0, 1) {
		out.ByDay = append(out.ByDay, DayCount{Date: d.Format(time.DateOnly)})
	}
	if staff {
		none := []HouseCount{}
		out.TopHouses = &none
	}
	return out
}

func uuidOrNull(u *uuid.UUID) pgtype.UUID {
	if u == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *u, Valid: true}
}
