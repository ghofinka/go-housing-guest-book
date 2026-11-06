package e2e

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
)

type statsBody struct {
	Period struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"period"`
	HouseID              *string  `json:"house_id"`
	Total                int      `json:"total"`
	StillInside          int      `json:"still_inside"`
	Overstay             int      `json:"overstay"`
	AverageMinutesInside *float64 `json:"average_minutes_inside"`
	ByCategory           []struct {
		Category string `json:"category"`
		Count    int    `json:"count"`
	} `json:"by_category"`
	ByDay []struct {
		Date  string `json:"date"`
		Count int    `json:"count"`
	} `json:"by_day"`
	ByHour []struct {
		Hour  int `json:"hour"`
		Count int `json:"count"`
	} `json:"by_hour"`
	BusiestHour *struct {
		Hour  int `json:"hour"`
		Count int `json:"count"`
	} `json:"busiest_hour"`
	TopHouses *[]struct {
		House struct {
			ID     string `json:"id"`
			Block  string `json:"block"`
			Number string `json:"number"`
		} `json:"house"`
		Count int `json:"count"`
	} `json:"top_houses"`
}

// mar is a moment in March 2025 in WIB. That month is used by no other test, so the totals below are
// exact even without a house filter.
func mar(day, hour, min, sec int) string {
	return fmt.Sprintf("2025-03-%02d %02d:%02d:%02d+07:00", day, hour, min, sec)
}

func insertVisitAt(t *testing.T, house, guardID, category, status, name, checkin, checkout string) {
	t.Helper()
	var out any
	if checkout != "" {
		out = checkout
	}
	if _, err := owner.Exec(context.Background(),
		`INSERT INTO visits (house_id, guest_name, category, checkin_by, status, checkin_at, checkout_at)
		 VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $7::timestamptz)`,
		house, name, category, guardID, status, checkin, out); err != nil {
		t.Fatal(err)
	}
}

type statsFixture struct {
	tag                          string
	houseA, houseB               string
	admin, guard, ownerA, ownerB tokenPair
}

func newStatsFixture(t *testing.T, tag string) statsFixture {
	t.Helper()
	em := strings.ToLower(tag)
	f := statsFixture{tag: tag, houseA: newHouse(t, "S"+tag, "A", true), houseB: newHouse(t, "S"+tag, "B", true)}
	seed(t, "Admin "+tag, "admin-st-"+em+"@example.test", "admin", nil)
	seed(t, "Guard "+tag, "guard-st-"+em+"@example.test", "guard", nil)
	seed(t, "Owner "+tag+"A", "owner-st-"+em+"a@example.test", "owner", f.houseA)
	seed(t, "Owner "+tag+"B", "owner-st-"+em+"b@example.test", "owner", f.houseB)
	f.admin, f.guard = login(t, "admin-st-"+em+"@example.test"), login(t, "guard-st-"+em+"@example.test")
	f.ownerA, f.ownerB = login(t, "owner-st-"+em+"a@example.test"), login(t, "owner-st-"+em+"b@example.test")
	return f
}

func (f statsFixture) get(t *testing.T, token, query string) statsBody {
	t.Helper()
	rec := do(t, "GET", "/v1/stats/visits"+query, token, nil)
	assertStatus(t, rec, http.StatusOK, "stats "+query)
	return decode[statsBody](t, rec)
}

const marPeriod = "?from=2025-03-01&to=2025-03-07"

// seedMarch fills March 2025: house A has five visits in the period (two of them either side of a WIB
// midnight), two just outside it, house B two, and ten other houses one each.
func (f statsFixture) seedMarch(t *testing.T) {
	t.Helper()
	// Start from an empty window, so the totals are exact whichever order the tests run in, and however
	// often (the window is used by no other test, and nothing refers to these rows).
	if _, err := owner.Exec(context.Background(),
		`DELETE FROM visits WHERE checkin_at >= '2025-02-28 00:00:00+07' AND checkin_at < '2025-03-09 00:00:00+07'`); err != nil {
		t.Fatal(err)
	}
	g := f.guard.User.ID
	insertVisitAt(t, f.houseA, g, "tamu", "exited", "Zq Stat 1", mar(1, 8, 30, 0), mar(1, 9, 30, 0))    // 60 min
	insertVisitAt(t, f.houseA, g, "kurir", "exited", "Zq Stat 2", mar(1, 14, 5, 0), mar(1, 14, 15, 0))  // 10 min
	insertVisitAt(t, f.houseA, g, "tamu", "exited", "Zq Stat 3", mar(2, 23, 59, 0), mar(3, 0, 29, 0))   // 30 min, crosses midnight
	insertVisitAt(t, f.houseA, g, "tukang", "inside", "Zq Stat 4", mar(3, 0, 0, 0), "")                 // first second of a WIB day
	insertVisitAt(t, f.houseA, g, "tamu", "inside", "Zq Stat 5", mar(7, 23, 59, 59), "")                // last second of the period
	insertVisitAt(t, f.houseA, g, "tamu", "inside", "Zq Stat Sebelum", "2025-02-28 23:59:59+07:00", "") // outside
	insertVisitAt(t, f.houseA, g, "tamu", "inside", "Zq Stat Sesudah", "2025-03-08 00:00:00+07:00", "") // outside
	insertVisitAt(t, f.houseB, g, "ojol", "exited", "Zq Stat B1", mar(4, 14, 10, 0), mar(4, 14, 40, 0)) // 30 min
	insertVisitAt(t, f.houseB, g, "ojol", "exited", "Zq Stat B2", mar(4, 14, 20, 0), mar(4, 14, 50, 0)) // 30 min
	for i := 0; i < 10; i++ {
		h := newHouse(t, fmt.Sprintf("X%02d", i), f.tag, true) // the number is the fixture's tag: names must be unique
		insertVisitAt(t, h, g, "tamu", "exited", "Zq Stat X", mar(5, 10, i, 0), mar(5, 10, i+15, 0))
	}
}

func counts(s statsBody) (cat []int, day []int, hour map[int]int) {
	for _, c := range s.ByCategory {
		cat = append(cat, c.Count)
	}
	for _, d := range s.ByDay {
		day = append(day, d.Count)
	}
	hour = map[int]int{}
	for _, h := range s.ByHour {
		if h.Count > 0 {
			hour[h.Hour] = h.Count
		}
	}
	return
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// C4: the figures, in WIB, with every day, hour and category present.
func TestStatsFigures(t *testing.T) {
	f := newStatsFixture(t, "SF")
	f.seedMarch(t)

	a := f.get(t, f.admin.AccessToken, marPeriod+"&house_id="+f.houseA)
	if a.Total != 5 || a.StillInside != 2 || a.Overstay != 2 {
		t.Fatalf("house A: total=%d inside=%d overstay=%d, want 5, 2 and 2 (a visit outside the period must not count)", a.Total, a.StillInside, a.Overstay)
	}
	if a.AverageMinutesInside == nil || math.Abs(*a.AverageMinutesInside-33.3) > 0.001 {
		t.Fatalf("average minutes inside = %v, want 33.3 (60, 10 and 30 minutes)", a.AverageMinutesInside)
	}
	cat, day, hour := counts(a)
	if want := []int{3, 1, 0, 1, 0, 0}; !eqInts(cat, want) {
		t.Fatalf("by category = %v, want %v (tamu, kurir, ojol, tukang, keluarga, lainnya)", cat, want)
	}
	wantCats := []string{"tamu", "kurir", "ojol", "tukang", "keluarga", "lainnya"}
	for i, c := range a.ByCategory {
		if c.Category != wantCats[i] {
			t.Fatalf("category %d is %q, want %q (a fixed order)", i, c.Category, wantCats[i])
		}
	}
	// Every day of the period is there, including the empty ones.
	if len(a.ByDay) != 7 {
		t.Fatalf("by day has %d entries, want one for each of the 7 days (empty days included): %v", len(a.ByDay), day)
	}
	// A WIB day: 00:00 on 3 March is still 3 March, not 2 March in UTC.
	if want := []int{2, 1, 1, 0, 0, 0, 1}; !eqInts(day, want) || a.ByDay[0].Date != "2025-03-01" || a.ByDay[6].Date != "2025-03-07" {
		t.Fatalf("by day = %v (first %s, last %s), want %v from 2025-03-01 to 2025-03-07", day, a.ByDay[0].Date, a.ByDay[6].Date, want)
	}
	if len(a.ByHour) != 24 {
		t.Fatalf("by hour has %d entries, want 24", len(a.ByHour))
	}
	for h, want := range map[int]int{0: 1, 8: 1, 14: 1, 23: 2} {
		if hour[h] != want {
			t.Fatalf("hour %d = %d, want %d (by hour: %v)", h, hour[h], want, hour)
		}
	}
	if len(hour) != 4 {
		t.Fatalf("by hour has other hours filled: %v", hour)
	}
	if a.BusiestHour == nil || a.BusiestHour.Hour != 23 || a.BusiestHour.Count != 2 {
		t.Fatalf("busiest hour = %+v, want hour 23 with 2", a.BusiestHour)
	}
	if a.TopHouses == nil || len(*a.TopHouses) != 1 || (*a.TopHouses)[0].House.ID != f.houseA || (*a.TopHouses)[0].Count != 5 {
		t.Fatalf("top houses with a house filter = %+v, want only house A with 5", a.TopHouses)
	}
	if a.HouseID == nil || *a.HouseID != f.houseA || a.Period.From != "2025-03-01" || a.Period.To != "2025-03-07" {
		t.Fatalf("echoed scope = house %v period %+v", a.HouseID, a.Period)
	}

	// House B: two ojol who stayed 30 minutes, nobody inside.
	b := f.get(t, f.admin.AccessToken, marPeriod+"&house_id="+f.houseB)
	if b.Total != 2 || b.StillInside != 0 || b.AverageMinutesInside == nil || *b.AverageMinutesInside != 30 || b.BusiestHour == nil || b.BusiestHour.Hour != 14 {
		t.Fatalf("house B = %+v", b)
	}

	// Every house: house A's 5 + B's 2 + the ten singles. The list is cut at ten, most first, ties by block.
	all := f.get(t, f.admin.AccessToken, marPeriod)
	if all.Total != 17 {
		t.Fatalf("all houses total = %d, want 17", all.Total)
	}
	top := *all.TopHouses
	if len(top) != 10 || top[0].House.ID != f.houseA || top[0].Count != 5 || top[1].House.ID != f.houseB || top[1].Count != 2 {
		t.Fatalf("top houses = %+v, want 10 entries starting with house A (5) and house B (2)", top)
	}
	for i := 2; i < 10; i++ {
		if top[i].Count != 1 || top[i].House.Block != fmt.Sprintf("X%02d", i-2) {
			t.Fatalf("top[%d] = %+v, want a single visit from block X%02d (ties are ordered by block)", i, top[i], i-2)
		}
	}
	// The same figures as the report for the same period: the two never disagree.
	csvRec := do(t, "GET", "/v1/reports/visits.csv"+marPeriod, f.admin.AccessToken, nil)
	assertStatus(t, csvRec, http.StatusOK, "report for the same period")
	if _, rows := parseCSV(t, csvRec); len(rows) != all.Total {
		t.Fatalf("the report has %d rows, the statistics say %d", len(rows), all.Total)
	}
	// And they hold no personal data.
	raw := do(t, "GET", "/v1/stats/visits"+marPeriod, f.admin.AccessToken, nil).Body.String()
	if strings.Contains(raw, "Zq") || strings.Contains(strings.ToLower(raw), "guest_name") || strings.Contains(strings.ToLower(raw), "plate") {
		t.Fatalf("the statistics contain personal data: %.300s", raw)
	}

	// An empty period still answers in full: every day, hour and category with 0, and no average.
	none := f.get(t, f.admin.AccessToken, "?from=2020-01-01&to=2020-01-07&house_id="+f.houseA)
	cat, day, hour = counts(none)
	if none.Total != 0 || len(hour) != 0 || !eqInts(day, make([]int, 7)) || !eqInts(cat, make([]int, 6)) || len(none.ByHour) != 24 {
		t.Fatalf("an empty period = %+v", none)
	}
	if none.BusiestHour != nil || none.AverageMinutesInside != nil || none.TopHouses == nil || len(*none.TopHouses) != 0 {
		t.Fatalf("an empty period should have no busiest hour or average and an empty (not missing) top list: %+v", none)
	}
	// Statistics are not audited: they are aggregates, loaded on every login, with no personal data in them.
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'stats%'`); n != 0 {
		t.Fatalf("%d statistics audit rows, want 0", n)
	}
}

// C4: each role gets its own scope. An owner sees only their house and no ranking of houses.
func TestStatsScopeByRole(t *testing.T) {
	f := newStatsFixture(t, "SR")
	f.seedMarch(t)

	oa := f.get(t, f.ownerA.AccessToken, marPeriod)
	if oa.Total != 5 || oa.StillInside != 2 || oa.HouseID == nil || *oa.HouseID != f.houseA {
		t.Fatalf("owner A = total %d inside %d house %v, want house A's own 5 and 2", oa.Total, oa.StillInside, oa.HouseID)
	}
	if oa.TopHouses != nil {
		t.Fatalf("owner A got a ranking of houses: %+v", *oa.TopHouses)
	}
	raw := do(t, "GET", "/v1/stats/visits"+marPeriod, f.ownerA.AccessToken, nil).Body.String()
	if strings.Contains(raw, "top_houses") {
		t.Fatal("the owner's answer contains the top_houses key")
	}
	ob := f.get(t, f.ownerB.AccessToken, marPeriod)
	if ob.Total != 2 || ob.TopHouses != nil {
		t.Fatalf("owner B = %+v, want house B's own 2 and no ranking", ob)
	}

	// Asking for another house matches nothing: no error, and nothing of that house.
	other := f.get(t, f.ownerA.AccessToken, marPeriod+"&house_id="+f.houseB)
	cat, day, _ := counts(other)
	if other.Total != 0 || !eqInts(cat, make([]int, 6)) || !eqInts(day, make([]int, 7)) || other.TopHouses != nil {
		t.Fatalf("owner A asking for house B = %+v, want empty", other)
	}
	// Their own house by id is fine and the same as leaving it out.
	if own := f.get(t, f.ownerA.AccessToken, marPeriod+"&house_id="+f.houseA); own.Total != 5 {
		t.Fatalf("owner A asking for their own house = %d, want 5", own.Total)
	}

	// A guard sees every house, like the guard's visit list.
	g := f.get(t, f.guard.AccessToken, marPeriod)
	adm := f.get(t, f.admin.AccessToken, marPeriod)
	if g.Total != adm.Total || g.TopHouses == nil || len(*g.TopHouses) != 10 {
		t.Fatalf("guard total %d (admin %d), top %v", g.Total, adm.Total, g.TopHouses)
	}

	assertStatus(t, do(t, "GET", "/v1/stats/visits"+marPeriod, "", nil), http.StatusUnauthorized, "anonymous")
}

// C4: the page can load right after login: with no dates the period is today and the six days before it.
func TestStatsDefaultPeriod(t *testing.T) {
	f := newStatsFixture(t, "SD")
	// A visit made now, through the API, so "today" has something in it.
	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"house_id": f.houseA, "guest_name": "Zq Hari Ini", "category": "tamu"})
	assertStatus(t, rec, http.StatusCreated, "check-in today")

	for name, tok := range map[string]string{"admin": f.admin.AccessToken, "guard": f.guard.AccessToken, "owner": f.ownerA.AccessToken} {
		s := f.get(t, tok, "")
		if s.Period.To != day(0) || s.Period.From != day(-6) || len(s.ByDay) != 7 {
			t.Fatalf("%s default period = %+v with %d days, want %s to %s", name, s.Period, len(s.ByDay), day(-6), day(0))
		}
		if s.ByDay[6].Date != day(0) || s.ByDay[6].Count < 1 {
			t.Fatalf("%s: today = %+v, want at least the visit just made", name, s.ByDay[6])
		}
	}
	// The owner's default answer is their own house, exactly the visit above.
	if own := f.get(t, f.ownerA.AccessToken, ""); own.Total != 1 || own.StillInside != 1 || own.ByDay[6].Count != 1 {
		t.Fatalf("owner A today = %+v, want exactly the one visit", own)
	}
	// Only one end given: the other is worked out.
	if s := f.get(t, f.admin.AccessToken, "?from="+day(-2)); s.Period.From != day(-2) || s.Period.To != day(0) || len(s.ByDay) != 3 {
		t.Fatalf("from only = %+v, want %s to today (3 days)", s.Period, day(-2))
	}
	if s := f.get(t, f.admin.AccessToken, "?to="+day(-10)); s.Period.To != day(-10) || s.Period.From != day(-16) || len(s.ByDay) != 7 {
		t.Fatalf("to only = %+v, want the seven days up to %s", s.Period, day(-10))
	}
	// A period that reaches into the future is fine.
	if s := f.get(t, f.admin.AccessToken, "?from="+day(0)+"&to="+day(5)); len(s.ByDay) != 6 {
		t.Fatalf("a window into the future has %d days, want 6", len(s.ByDay))
	}
}

func TestStatsValidation(t *testing.T) {
	f := newStatsFixture(t, "SV")
	for name, q := range map[string]string{
		"inverted":      "?from=2025-03-07&to=2025-03-01",
		"bad from":      "?from=besok",
		"bad to":        "?from=2025-03-01&to=kemarin",
		"bad house":     "?from=2025-03-01&to=2025-03-07&house_id=nope",
		"94 days":       "?from=2025-01-01&to=2025-04-04",
		"to alone, old": "?to=2024-01-01&from=2025-03-01",
	} {
		for _, role := range []tokenPair{f.admin, f.guard, f.ownerA} {
			assertStatus(t, do(t, "GET", "/v1/stats/visits"+q, role.AccessToken, nil), http.StatusUnprocessableEntity, name)
		}
	}
	// 93 days, inclusive, is the longest allowed.
	s := f.get(t, f.admin.AccessToken, "?from=2025-01-01&to=2025-04-03")
	if len(s.ByDay) != 93 {
		t.Fatalf("a 93-day period has %d days, want 93", len(s.ByDay))
	}
}
