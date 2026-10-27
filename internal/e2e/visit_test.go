package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

type visitBody struct {
	ID    string `json:"id"`
	House struct {
		ID     string `json:"id"`
		Block  string `json:"block"`
		Number string `json:"number"`
	} `json:"house"`
	GuestName    *string `json:"guest_name"`
	Category     string  `json:"category"`
	VehiclePlate *string `json:"vehicle_plate"`
	CheckinAt    string  `json:"checkin_at"`
	CheckinBy    string  `json:"checkin_by"`
	CheckoutAt   *string `json:"checkout_at"`
	CheckoutBy   *string `json:"checkout_by"`
	Status       string  `json:"status"`
}

type visitList struct {
	Data       []visitBody `json:"data"`
	NextCursor *string     `json:"next_cursor"`
}

// newHouse inserts an active house as the table owner and returns its id.
func newHouse(t *testing.T, block, number string, active bool) string {
	t.Helper()
	var id string
	if err := owner.QueryRow(context.Background(),
		`INSERT INTO houses (block, number, active) VALUES ($1,$2,$3) RETURNING id`, block, number, active).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func dbNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := owner.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func TestVisitAccessControl(t *testing.T) {
	seed(t, "Admin V", "admin-v@example.test", "admin", nil)
	seed(t, "Guard V", "guard-v@example.test", "guard", nil)
	houseID := newHouse(t, "V", "1", true)
	seed(t, "Owner V", "owner-v@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-v@example.test"), login(t, "guard-v@example.test"), login(t, "owner-v@example.test")

	v := decode[visitBody](t, do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "Tamu V", "category": "tamu"}))
	body := map[string]any{"house_id": houseID, "guest_name": "X", "category": "tamu"}

	assertStatus(t, do(t, "POST", "/v1/visits", "", body), http.StatusUnauthorized, "anonymous check-in")
	assertStatus(t, do(t, "GET", "/v1/visits", "", nil), http.StatusUnauthorized, "anonymous list")

	// Owners cannot write to the gate log; they may read their own house (see TestOwnerSeesOnlyOwnHouse).
	assertStatus(t, do(t, "POST", "/v1/visits", own.AccessToken, body), http.StatusForbidden, "owner check-in")
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", own.AccessToken, nil), http.StatusForbidden, "owner check-out")
	assertStatus(t, do(t, "GET", "/v1/visits", own.AccessToken, nil), http.StatusOK, "owner list")
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID, own.AccessToken, nil), http.StatusOK, "owner get own house's visit")

	// Admins can read but not write the gate log.
	assertStatus(t, do(t, "POST", "/v1/visits", admin.AccessToken, body), http.StatusForbidden, "admin check-in")
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", admin.AccessToken, nil), http.StatusForbidden, "admin check-out")
	assertStatus(t, do(t, "GET", "/v1/visits", admin.AccessToken, nil), http.StatusOK, "admin list")
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID, admin.AccessToken, nil), http.StatusOK, "admin get")
}

func TestCheckIn(t *testing.T) {
	seed(t, "Guard C1", "guard-ci@example.test", "guard", nil)
	guard := login(t, "guard-ci@example.test")
	houseID := newHouse(t, "CI", "7", true)
	inactiveID := newHouse(t, "CI", "8", false)

	before := dbNow(t)
	rec := doForm(t, "/v1/visits", guard.AccessToken, map[string]string{
		"house_id": houseID, "guest_name": "  Budi Test ", "category": "tamu",
		"purpose_note": "Antar dokumen", "vehicle_plate": " b  1234   xyz ",
	}, map[string][]byte{"vehicle_photo": testJPEG(t, 64, 48, "")})
	assertStatus(t, rec, http.StatusCreated, "check-in")
	after := dbNow(t)
	v := decode[visitBody](t, rec)

	if v.Status != "inside" || v.CheckoutAt != nil || v.CheckoutBy != nil {
		t.Fatalf("new visit should be inside: %+v", v)
	}
	if v.GuestName == nil || *v.GuestName != "Budi Test" {
		t.Fatalf("name not trimmed: %v", v.GuestName)
	}
	if v.VehiclePlate == nil || *v.VehiclePlate != "B 1234 XYZ" {
		t.Fatalf("plate not normalized: %v", v.VehiclePlate)
	}
	if v.CheckinBy != guard.User.ID {
		t.Fatalf("checkin_by = %s, want the logged-in guard %s", v.CheckinBy, guard.User.ID)
	}
	if v.House.Block != "CI" || v.House.Number != "7" {
		t.Fatalf("house = %+v", v.House)
	}
	// M2: time comes from the server clock, rendered in WIB.
	at, err := time.Parse(time.RFC3339Nano, v.CheckinAt)
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(after.Add(time.Second)) {
		t.Fatalf("checkin_at %q (%v) not between %v and %v", v.CheckinAt, err, before, after)
	}
	if !strings.HasSuffix(v.CheckinAt, "+07:00") {
		t.Fatalf("checkin_at %q is not in WIB", v.CheckinAt)
	}
	// ...and the client cannot supply it.
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken, map[string]any{
		"house_id": houseID, "guest_name": "X", "category": "tamu", "checkin_at": "2020-01-01T00:00:00Z"}),
		http.StatusBadRequest, "client-supplied checkin_at")

	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'visit_checked_in'`, "visit:"+v.ID); n != 1 {
		t.Fatalf("visit_checked_in audit rows = %d, want 1", n)
	}

	// Courier and ojol fast lane: name optional, no plate needed.
	for _, cat := range []string{"kurir", "ojol"} {
		rec := do(t, "POST", "/v1/visits", guard.AccessToken, map[string]any{"house_id": houseID, "category": cat})
		assertStatus(t, rec, http.StatusCreated, cat+" without name")
		if g := decode[visitBody](t, rec).GuestName; g != nil {
			t.Fatalf("%s guest_name = %q, want null", cat, *g)
		}
	}

	long := strings.Repeat("a", 101)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing house", map[string]any{"guest_name": "X", "category": "tamu"}},
		{"unknown house", map[string]any{"house_id": uuid.NewString(), "guest_name": "X", "category": "tamu"}},
		{"inactive house", map[string]any{"house_id": inactiveID, "guest_name": "X", "category": "tamu"}},
		{"bad category", map[string]any{"house_id": houseID, "guest_name": "X", "category": "vip"}},
		{"missing category", map[string]any{"house_id": houseID, "guest_name": "X"}},
		{"tamu without name", map[string]any{"house_id": houseID, "category": "tamu"}},
		{"blank name", map[string]any{"house_id": houseID, "guest_name": "   ", "category": "tukang"}},
		{"name too long", map[string]any{"house_id": houseID, "guest_name": long, "category": "tamu"}},
		{"note too long", map[string]any{"house_id": houseID, "guest_name": "X", "category": "tamu", "purpose_note": strings.Repeat("n", 201)}},
		{"plate with symbols", map[string]any{"house_id": houseID, "guest_name": "X", "category": "tamu", "vehicle_plate": "B-1234"}},
		{"plate too long", map[string]any{"house_id": houseID, "guest_name": "X", "category": "tamu", "vehicle_plate": "B 1234 ABCDEFGH"}},
	}
	for _, c := range cases {
		if rec := do(t, "POST", "/v1/visits", guard.AccessToken, c.body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (%s)", c.name, rec.Code, rec.Body)
		}
	}
}

func TestCheckInIsIdempotent(t *testing.T) {
	seed(t, "Guard Idem", "guard-idem@example.test", "guard", nil)
	guard := login(t, "guard-idem@example.test")
	houseID := newHouse(t, "ID", "1", true)
	key := uuid.NewString()
	body := map[string]any{"client_request_id": key, "house_id": houseID, "guest_name": "Retry Test", "category": "tamu"}

	first := do(t, "POST", "/v1/visits", guard.AccessToken, body)
	assertStatus(t, first, http.StatusCreated, "first")
	second := do(t, "POST", "/v1/visits", guard.AccessToken, body)
	assertStatus(t, second, http.StatusOK, "retry")
	a, b := decode[visitBody](t, first), decode[visitBody](t, second)
	if a.ID != b.ID || a.CheckinAt != b.CheckinAt {
		t.Fatalf("retry returned a different visit: %+v vs %+v", a, b)
	}
	if n := count(t, `SELECT count(*) FROM visits WHERE client_request_id = $1`, key); n != 1 {
		t.Fatalf("rows for key = %d, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1`, "visit:"+a.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (retry must not re-audit)", n)
	}

	// A retry still succeeds after the house is deactivated.
	owner.Exec(context.Background(), `UPDATE houses SET active = false WHERE id = $1`, houseID)
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken, body), http.StatusOK, "retry after house deactivated")
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "New", "category": "tamu"}), http.StatusUnprocessableEntity, "new check-in to deactivated house")
}

func TestCheckOut(t *testing.T) {
	seed(t, "Guard Out", "guard-out@example.test", "guard", nil)
	seed(t, "Guard Out 2", "guard-out2@example.test", "guard", nil)
	guard, guard2 := login(t, "guard-out@example.test"), login(t, "guard-out2@example.test")
	houseID := newHouse(t, "OUT", "1", true)

	v := decode[visitBody](t, do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "Pulang Test", "category": "tamu"}))

	before := dbNow(t)
	rec := do(t, "POST", "/v1/visits/"+v.ID+"/checkout", guard2.AccessToken, nil)
	assertStatus(t, rec, http.StatusOK, "check-out")
	after := dbNow(t)
	out := decode[visitBody](t, rec)
	if out.Status != "exited" || out.CheckoutAt == nil || out.CheckoutBy == nil || *out.CheckoutBy != guard2.User.ID {
		t.Fatalf("check-out result: %+v", out)
	}
	at, err := time.Parse(time.RFC3339Nano, *out.CheckoutAt)
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(after.Add(time.Second)) {
		t.Fatalf("checkout_at %q not from the server clock (%v)", *out.CheckoutAt, err)
	}
	if !strings.HasSuffix(*out.CheckoutAt, "+07:00") {
		t.Fatalf("checkout_at %q is not in WIB", *out.CheckoutAt)
	}
	if out.CheckinAt != v.CheckinAt || out.CheckinBy != guard.User.ID {
		t.Fatal("check-out altered check-in data")
	}

	// Idempotent: a second tap, even by another guard, keeps the original exit.
	again := decode[visitBody](t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", guard.AccessToken, nil))
	if again.CheckoutAt == nil || *again.CheckoutAt != *out.CheckoutAt || *again.CheckoutBy != guard2.User.ID {
		t.Fatalf("second check-out changed the record: %+v", again)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'visit_checked_out'`, "visit:"+v.ID); n != 1 {
		t.Fatalf("visit_checked_out audit rows = %d, want 1", n)
	}

	assertStatus(t, do(t, "POST", "/v1/visits/"+uuid.NewString()+"/checkout", guard.AccessToken, nil), http.StatusNotFound, "unknown visit")
	assertStatus(t, do(t, "POST", "/v1/visits/not-a-uuid/checkout", guard.AccessToken, nil), http.StatusNotFound, "malformed id")

	// It moves from "inside" to "exited" in the lists.
	inside := decode[visitList](t, do(t, "GET", "/v1/visits?status=inside&house_id="+houseID, guard.AccessToken, nil))
	exited := decode[visitList](t, do(t, "GET", "/v1/visits?status=exited&house_id="+houseID, guard.AccessToken, nil))
	if len(inside.Data) != 0 || len(exited.Data) != 1 || exited.Data[0].ID != v.ID {
		t.Fatalf("inside=%d exited=%+v", len(inside.Data), exited.Data)
	}
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID, guard.AccessToken, nil), http.StatusOK, "get")
}

func TestGuestsInsideList(t *testing.T) {
	seed(t, "Guard L", "guard-l@example.test", "guard", nil)
	guard := login(t, "guard-l@example.test")
	houseA, houseB := newHouse(t, "L", "1", true), newHouse(t, "L", "2", true)

	var ids []string // oldest first
	for i, h := range []string{houseA, houseA, houseB, houseA, houseB} {
		v := decode[visitBody](t, do(t, "POST", "/v1/visits", guard.AccessToken,
			map[string]any{"house_id": h, "guest_name": "Guest " + string(rune('A'+i)), "category": "tamu"}))
		ids = append(ids, v.ID)
	}
	// One of house A's guests leaves.
	assertStatus(t, do(t, "POST", "/v1/visits/"+ids[0]+"/checkout", guard.AccessToken, nil), http.StatusOK, "checkout")

	// Newest first, only those still inside, filtered by house, paged one at a time.
	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		path := "/v1/visits?status=inside&limit=1&house_id=" + houseA
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := decode[visitList](t, do(t, "GET", path, guard.AccessToken, nil))
		for _, v := range res.Data {
			got = append(got, v.ID)
		}
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
	}
	if want := []string{ids[3], ids[1]}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("house A inside = %v, want %v", got, want)
	}

	assertStatus(t, do(t, "GET", "/v1/visits?status=bogus", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad status")
	assertStatus(t, do(t, "GET", "/v1/visits?limit=0", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad limit")
	assertStatus(t, do(t, "GET", "/v1/visits?cursor=garbage", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad cursor")
	assertStatus(t, do(t, "GET", "/v1/visits?status=inside&cursor=garbage", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad cursor (inside)")
}

// The database itself must enforce house isolation, independent of any filter in Go.
func TestVisitRowLevelSecurity(t *testing.T) {
	seed(t, "Guard R", "guard-r@example.test", "guard", nil)
	guard := login(t, "guard-r@example.test")
	houseA, houseB := newHouse(t, "RLS", "A", true), newHouse(t, "RLS", "B", true)
	for _, h := range []string{houseA, houseA, houseB} {
		assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken,
			map[string]any{"house_id": h, "guest_name": "RLS", "category": "tamu"}), http.StatusCreated, "seed visit")
	}

	countAs := func(role string, house *uuid.UUID) (n int) {
		t.Helper()
		err := db.InTx(context.Background(), app, role, house, func(q *db.Queries) error {
			rows, err := q.ListVisits(context.Background(), db.ListVisitsParams{PageSize: 100})
			for _, r := range rows {
				if r.Block == "RLS" {
					n++
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	a, b := uuid.MustParse(houseA), uuid.MustParse(houseB)
	if got := countAs("owner", &a); got != 2 {
		t.Errorf("owner of A sees %d visits, want 2", got)
	}
	if got := countAs("owner", &b); got != 1 {
		t.Errorf("owner of B sees %d visits, want 1", got)
	}
	if got := countAs("owner", nil); got != 0 {
		t.Errorf("owner without a house sees %d visits, want 0", got)
	}
	if got := countAs("", nil); got != 0 {
		t.Errorf("no role sees %d visits, want 0", got)
	}
	if got := countAs("guard", nil); got != 3 {
		t.Errorf("guard sees %d visits, want 3", got)
	}

	// An owner cannot write visits at all, even via SQL.
	err := db.InTx(context.Background(), app, "owner", &a, func(q *db.Queries) error {
		_, err := q.CreateVisit(context.Background(), db.CreateVisitParams{
			HouseID: a, Category: "tamu", CheckinBy: uuid.MustParse(guard.User.ID)})
		return err
	})
	if err == nil {
		t.Error("owner role was able to insert a visit")
	}
	// A plain connection with no RLS context sees nothing.
	var n int
	if err := app.QueryRow(context.Background(), `SELECT count(*) FROM visits`).Scan(&n); err != nil || n != 0 {
		t.Errorf("app role without context sees %d visits (err=%v), want 0", n, err)
	}
}
