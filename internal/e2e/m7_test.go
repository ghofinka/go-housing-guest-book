package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"
)

var wibZone = time.FixedZone("WIB", 7*60*60)

// M7: the owner's history of their own house, newest first, filterable by date.
func TestOwnerHistory(t *testing.T) {
	f := newOwnerFixture(t, "HI")
	seed(t, "Admin HI", "admin-hi@example.test", "admin", nil)
	admin := login(t, "admin-hi@example.test")

	// Two more visits for house A, then pin all four to known WIB times that straddle midnight.
	extra := func(name string) string {
		rec := do(t, "POST", "/v1/visits", f.guard.AccessToken,
			map[string]any{"house_id": f.houseA, "guest_name": name, "category": "tukang", "purpose_note": "Servis AC"})
		assertStatus(t, rec, http.StatusCreated, "extra visit")
		return decode[visitBody](t, rec).ID
	}
	times := []struct {
		id string
		at time.Time
	}{
		{f.visitsA[0].ID, time.Date(2026, 3, 9, 23, 30, 0, 0, wibZone)}, // late on the 9th
		{f.visitsA[1].ID, time.Date(2026, 3, 10, 0, 30, 0, 0, wibZone)}, // just after midnight on the 10th
		{extra("Tukang C"), time.Date(2026, 3, 10, 18, 0, 0, 0, wibZone)},
		{extra("Tukang D"), time.Date(2026, 3, 11, 8, 0, 0, 0, wibZone)},
		{f.visitB.ID, time.Date(2026, 3, 10, 12, 0, 0, 0, wibZone)}, // another house, same day
	}
	for _, v := range times {
		if _, err := owner.Exec(context.Background(), `UPDATE visits SET checkin_at = $1 WHERE id = $2`, v.at, v.id); err != nil {
			t.Fatal(err)
		}
	}
	idAt := func(i int) string { return times[i].id }

	list := func(bearer, query string) visitList {
		t.Helper()
		rec := do(t, "GET", "/v1/visits"+query, bearer, nil)
		assertStatus(t, rec, http.StatusOK, "list "+query)
		return decode[visitList](t, rec)
	}
	equal := func(what string, got visitList, want ...string) {
		t.Helper()
		g := ids(got)
		if len(g) != len(want) {
			t.Fatalf("%s: got %v, want %v", what, g, want)
		}
		for i := range g {
			if g[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", what, g, want)
			}
		}
	}
	own := f.ownerA.AccessToken

	// Newest first, house A only.
	equal("everything", list(own, ""), idAt(3), idAt(2), idAt(1), idAt(0))

	// A single WIB day: includes 00:30 and 18:00 on the 10th, excludes 23:30 on the 9th and 08:00 on the 11th.
	equal("one day", list(own, "?from=2026-03-10&to=2026-03-10"), idAt(2), idAt(1))
	equal("from only", list(own, "?from=2026-03-10"), idAt(3), idAt(2), idAt(1))
	equal("to only", list(own, "?to=2026-03-10"), idAt(2), idAt(1), idAt(0))
	equal("from the 11th", list(own, "?from=2026-03-11"), idAt(3))
	equal("empty range", list(own, "?from=2026-04-01&to=2026-04-30"))
	equal("range across days", list(own, "?from=2026-03-09&to=2026-03-11"), idAt(3), idAt(2), idAt(1), idAt(0))

	// The date filter never widens access: another house's same-day visit stays hidden, and
	// the other owner sees only theirs.
	equal("owner B same day", list(f.ownerB.AccessToken, "?from=2026-03-10&to=2026-03-10"), idAt(4))
	equal("owner A asking for house B", list(own, "?from=2026-03-10&to=2026-03-10&house_id="+f.houseB))

	// Paging inside a date range keeps order and does not repeat or skip.
	var paged []string
	cursor := ""
	for i := 0; i < 6; i++ {
		q := "?from=2026-03-09&to=2026-03-11&limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		page := list(own, q)
		paged = append(paged, ids(page)...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if want := []string{idAt(3), idAt(2), idAt(1), idAt(0)}; len(paged) != 4 || paged[0] != want[0] || paged[1] != want[1] || paged[2] != want[2] || paged[3] != want[3] {
		t.Fatalf("paged = %v, want %v", paged, want)
	}

	// The status filter combines with dates; checking one out moves it to "exited".
	assertStatus(t, do(t, "POST", "/v1/visits/"+idAt(2)+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "check-out")
	equal("inside on the 10th", list(own, "?status=inside&from=2026-03-10&to=2026-03-10"), idAt(1))
	equal("exited on the 10th", list(own, "?status=exited&from=2026-03-10&to=2026-03-10"), idAt(2))

	// What the owner sees per visit: guest, times, purpose, photo link; not which guard was on duty.
	v := decode[visitBody](t, do(t, "GET", "/v1/visits/"+idAt(2), own, nil))
	if v.GuestName == nil || *v.GuestName != "Tukang C" || v.Category != "tukang" || v.CheckoutAt == nil || v.CheckinAt == "" {
		t.Fatalf("owner view is missing fields: %+v", v)
	}
	if v.CheckinBy != "" || v.CheckoutBy != nil {
		t.Fatalf("owner can see which guard was on duty: checkin_by=%q checkout_by=%v", v.CheckinBy, v.CheckoutBy)
	}
	fromList := list(own, "?from=2026-03-09&to=2026-03-09").Data[0]
	if fromList.CheckinBy != "" {
		t.Fatalf("list leaks checkin_by to the owner: %q", fromList.CheckinBy)
	}
	if fromList.House.ID == "" || fromList.VehiclePlate == nil {
		t.Fatalf("history row should carry the house and plate: %+v", fromList)
	}
	if fromList.CheckinAt == "" || !hasWIBOffset(fromList.CheckinAt) {
		t.Fatalf("check-in time %q is not in WIB", fromList.CheckinAt)
	}
	// Guards and admins still see who handled it.
	if g := decode[visitBody](t, do(t, "GET", "/v1/visits/"+idAt(2), f.guard.AccessToken, nil)); g.CheckinBy != f.guard.User.ID {
		t.Fatalf("guard lost checkin_by: %q", g.CheckinBy)
	}
	if a := decode[visitBody](t, do(t, "GET", "/v1/visits/"+idAt(2), admin.AccessToken, nil)); a.CheckinBy != f.guard.User.ID {
		t.Fatalf("admin lost checkin_by: %q", a.CheckinBy)
	}

	// Bad input.
	for name, q := range map[string]string{
		"bad from":        "?from=10-03-2026",
		"bad to":          "?to=tomorrow",
		"impossible date": "?from=2026-02-30",
		"to before from":  "?from=2026-03-11&to=2026-03-10",
	} {
		assertStatus(t, do(t, "GET", "/v1/visits"+q, own, nil), http.StatusUnprocessableEntity, name)
	}
}

func hasWIBOffset(ts string) bool { return len(ts) > 6 && ts[len(ts)-6:] == "+07:00" }
