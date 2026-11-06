package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type regGuest struct {
	ID    string `json:"id"`
	House struct {
		ID string `json:"id"`
	} `json:"house"`
	GuestName string  `json:"guest_name"`
	Plate     *string `json:"plate"`
	Category  string  `json:"category"`
	Active    bool    `json:"active"`
}

type regList struct {
	Data       []regGuest `json:"data"`
	NextCursor *string    `json:"next_cursor"`
}

type regVisit struct {
	ID    string `json:"id"`
	House struct {
		ID string `json:"id"`
	} `json:"house"`
	GuestName     *string `json:"guest_name"`
	VehiclePlate  *string `json:"vehicle_plate"`
	Category      string  `json:"category"`
	Status        string  `json:"status"`
	OwnerDecision struct {
		Status    string  `json:"status"`
		DecidedAt *string `json:"decided_at"`
		Source    *string `json:"source"`
	} `json:"owner_decision"`
}

type regFixture struct {
	houseA, houseB               string
	ownerA, ownerB, guard, admin tokenPair
}

func newRegFixture(t *testing.T, tag string) regFixture {
	t.Helper()
	em := strings.ToLower(tag)
	f := regFixture{houseA: newHouse(t, "G"+tag, "1", true), houseB: newHouse(t, "G"+tag, "2", true)}
	seed(t, "Owner "+tag+"A", "owner-g-"+em+"a@example.test", "owner", f.houseA)
	seed(t, "Owner "+tag+"B", "owner-g-"+em+"b@example.test", "owner", f.houseB)
	seed(t, "Guard "+tag, "guard-g-"+em+"@example.test", "guard", nil)
	seed(t, "Admin "+tag, "admin-g-"+em+"@example.test", "admin", nil)
	f.ownerA, f.ownerB = login(t, "owner-g-"+em+"a@example.test"), login(t, "owner-g-"+em+"b@example.test")
	f.guard, f.admin = login(t, "guard-g-"+em+"@example.test"), login(t, "admin-g-"+em+"@example.test")
	return f
}

func addRegular(t *testing.T, token string, body map[string]any) regGuest {
	t.Helper()
	rec := do(t, "POST", "/v1/regular-guests", token, body)
	assertStatus(t, rec, http.StatusCreated, fmt.Sprintf("add regular guest %v", body["guest_name"]))
	return decode[regGuest](t, rec)
}

func regularCheckIn(t *testing.T, token string, fields map[string]string, photo bool) *httptest.ResponseRecorder {
	t.Helper()
	files := map[string][]byte{}
	if photo {
		files["vehicle_photo"] = testJPEG(t, 60, 40, "")
	}
	return doForm(t, "/v1/visits", token, fields, files)
}

// C2: an owner keeps a list of regular guests for their own house; guards and admins can read it.
func TestRegularGuestsManagement(t *testing.T) {
	f := newRegFixture(t, "RM")

	maid := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "  Bu Siti ART  ", "plate": "b 1234 zqa"})
	if maid.GuestName != "Bu Siti ART" || maid.Plate == nil || *maid.Plate != "B 1234 ZQA" || maid.Category != "tamu" ||
		!maid.Active || maid.House.ID != f.houseA {
		t.Fatalf("maid = %+v, want a trimmed name, a normalized plate, the default category and house A", maid)
	}
	tutor := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "Pak Guru", "category": "tukang"})
	if tutor.Plate != nil || tutor.Category != "tukang" {
		t.Fatalf("tutor = %+v, want no plate and category tukang", tutor)
	}

	for name, body := range map[string]map[string]any{
		"missing name":      {},
		"blank name":        {"guest_name": "   "},
		"name too long":     {"guest_name": strings.Repeat("a", 101)},
		"plate with symbol": {"guest_name": "X", "plate": "B-1"},
		"plate too long":    {"guest_name": "X", "plate": "B 1234 ABCDEFGH"},
		"bad category":      {"guest_name": "X", "category": "vip"},
	} {
		assertStatus(t, do(t, "POST", "/v1/regular-guests", f.ownerA.AccessToken, body), http.StatusUnprocessableEntity, name)
	}

	// Only an owner writes. A guard and an admin read, so neither may create, change or delete.
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "admin": f.admin.AccessToken} {
		assertStatus(t, do(t, "POST", "/v1/regular-guests", tok, map[string]any{"guest_name": "X"}), http.StatusForbidden, name+" create")
		assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, tok, map[string]any{"guest_name": "X"}), http.StatusForbidden, name+" update")
		assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+maid.ID, tok, nil), http.StatusForbidden, name+" delete")
	}
	assertStatus(t, do(t, "POST", "/v1/regular-guests", "", map[string]any{"guest_name": "X"}), http.StatusUnauthorized, "anonymous create")

	// Lists: an owner sees their own house, whatever they ask for; guards and admins see every house.
	bGuest := addRegular(t, f.ownerB.AccessToken, map[string]any{"guest_name": "Tamu Rumah B"})
	ownA := decode[regList](t, do(t, "GET", "/v1/regular-guests?limit=200", f.ownerA.AccessToken, nil))
	if len(ownA.Data) != 2 {
		t.Fatalf("owner A sees %d regular guests, want 2", len(ownA.Data))
	}
	for _, g := range ownA.Data {
		if g.House.ID != f.houseA {
			t.Fatalf("owner A sees a guest of house %s", g.House.ID)
		}
	}
	other := decode[regList](t, do(t, "GET", "/v1/regular-guests?house_id="+f.houseB, f.ownerA.AccessToken, nil))
	if len(other.Data) != 0 {
		t.Fatalf("owner A asking for house B sees %d guests", len(other.Data))
	}
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "admin": f.admin.AccessToken} {
		got := decode[regList](t, do(t, "GET", "/v1/regular-guests?house_id="+f.houseB, tok, nil))
		if len(got.Data) != 1 || got.Data[0].ID != bGuest.ID {
			t.Fatalf("%s filtering by house B sees %+v, want the one guest", name, got.Data)
		}
	}
	page1 := decode[regList](t, do(t, "GET", "/v1/regular-guests?limit=1", f.ownerA.AccessToken, nil))
	if len(page1.Data) != 1 || page1.NextCursor == nil || page1.Data[0].ID != tutor.ID {
		t.Fatalf("page 1 = %+v, want the newest guest and a cursor", page1)
	}
	page2 := decode[regList](t, do(t, "GET", "/v1/regular-guests?limit=1&cursor="+*page1.NextCursor, f.ownerA.AccessToken, nil))
	if len(page2.Data) != 1 || page2.Data[0].ID != maid.ID || page2.NextCursor != nil {
		t.Fatalf("page 2 = %+v, want the older guest and no cursor", page2)
	}
	assertStatus(t, do(t, "GET", "/v1/regular-guests?active=maybe", f.ownerA.AccessToken, nil), http.StatusUnprocessableEntity, "bad active filter")
	assertStatus(t, do(t, "GET", "/v1/regular-guests?house_id=nope", f.guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad house filter")
	assertStatus(t, do(t, "GET", "/v1/regular-guests", "", nil), http.StatusUnauthorized, "anonymous list")

	// Update: name, plate (set, then removed with an empty string), category, active.
	up := decode[regGuest](t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, f.ownerA.AccessToken,
		map[string]any{"guest_name": "Bu Siti Baru", "plate": "b 9 zqb", "category": "keluarga"}))
	if up.GuestName != "Bu Siti Baru" || up.Plate == nil || *up.Plate != "B 9 ZQB" || up.Category != "keluarga" || !up.Active {
		t.Fatalf("after update: %+v", up)
	}
	cleared := decode[regGuest](t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, f.ownerA.AccessToken, map[string]any{"plate": ""}))
	if cleared.Plate != nil || cleared.GuestName != "Bu Siti Baru" {
		t.Fatalf("after clearing the plate: %+v, want no plate and the name kept", cleared)
	}
	off := decode[regGuest](t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, f.ownerA.AccessToken, map[string]any{"active": false}))
	if off.Active {
		t.Fatal("deactivating did not stick")
	}
	inactive := decode[regList](t, do(t, "GET", "/v1/regular-guests?active=false", f.ownerA.AccessToken, nil))
	if len(inactive.Data) != 1 || inactive.Data[0].ID != maid.ID {
		t.Fatalf("active=false returned %+v", inactive.Data)
	}
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, f.ownerA.AccessToken, map[string]any{}), http.StatusUnprocessableEntity, "empty update")
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+maid.ID, f.ownerA.AccessToken, map[string]any{"category": "vip"}), http.StatusUnprocessableEntity, "bad category update")
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/not-a-uuid", f.ownerA.AccessToken, map[string]any{"guest_name": "X"}), http.StatusNotFound, "malformed id")

	// Another house's entry answers exactly like a missing one, and nothing changes.
	missing := do(t, "PATCH", "/v1/regular-guests/"+uuid.NewString(), f.ownerB.AccessToken, map[string]any{"guest_name": "X"})
	cross := do(t, "PATCH", "/v1/regular-guests/"+tutor.ID, f.ownerB.AccessToken, map[string]any{"guest_name": "Diubah B"})
	assertStatus(t, cross, http.StatusNotFound, "owner B updates A's guest")
	if cross.Body.String() != missing.Body.String() {
		t.Fatalf("cross-house 404 differs from a missing id:\n%s\n%s", cross.Body, missing.Body)
	}
	assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+tutor.ID, f.ownerB.AccessToken, nil), http.StatusNotFound, "owner B deletes A's guest")
	if n := count(t, `SELECT count(*) FROM regular_guests WHERE id = $1 AND guest_name = 'Pak Guru'`, tutor.ID); n != 1 {
		t.Fatal("a cross-house change or delete reached the row")
	}

	// Delete for good, then again.
	assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+tutor.ID, f.ownerA.AccessToken, nil), http.StatusNoContent, "delete")
	assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+tutor.ID, f.ownerA.AccessToken, nil), http.StatusNotFound, "delete again")

	// Audit: by id only, and a guard reading the list is not recorded.
	for action, want := range map[string]int{"regular_guest_created": 2, "regular_guest_updated": 3, "regular_guest_deleted": 1} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = $1 AND user_id = $2`, action, f.ownerA.User.ID); n != want {
			t.Errorf("%s audit rows = %d, want %d", action, n, want)
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'regular_guests_listed' AND user_id = $1`, f.guard.User.ID); n != 0 {
		t.Fatalf("a guard's list was audited %d times, want 0", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'regular_guests_listed' AND user_id = $1`, f.admin.User.ID); n == 0 {
		t.Fatal("an admin's list was not audited")
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'regular_guest%' AND target NOT LIKE 'regular:%' AND target NOT LIKE 'house%'`); n != 0 {
		t.Fatalf("%d regular-guest audit rows have an unexpected target", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'regular_guest%' AND (target ILIKE '%siti%' OR target ILIKE '%zqa%' OR target ILIKE '%guru%')`); n != 0 {
		t.Fatalf("%d regular-guest audit rows contain a name or plate", n)
	}
}

// At most 20 active regular guests per house; switching one back on counts too.
func TestRegularGuestsAreCapped(t *testing.T) {
	f := newRegFixture(t, "RC")
	var first regGuest
	for i := 0; i < 20; i++ {
		g := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": fmt.Sprintf("Langganan %d", i)})
		if i == 0 {
			first = g
		}
	}
	assertStatus(t, do(t, "POST", "/v1/regular-guests", f.ownerA.AccessToken, map[string]any{"guest_name": "Ke-21"}), http.StatusConflict, "21st active guest")
	addRegular(t, f.ownerB.AccessToken, map[string]any{"guest_name": "Rumah Lain"}) // another house is unaffected

	// Deactivating frees a place; reactivating while full is refused; deleting frees a place too.
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+first.ID, f.ownerA.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate")
	extra := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "Pengganti"})
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+first.ID, f.ownerA.AccessToken, map[string]any{"active": true}), http.StatusConflict, "reactivate while full")
	assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+extra.ID, f.ownerA.AccessToken, nil), http.StatusNoContent, "delete one")
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+first.ID, f.ownerA.AccessToken, map[string]any{"active": true}), http.StatusOK, "reactivate with room")
}

// C2: the guard checks a regular guest in by picking the entry; the owner's approval counts as given.
func TestCheckInWithRegularGuest(t *testing.T) {
	f := newRegFixture(t, "RK")
	driver := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "Mas Budi Sopir", "category": "tukang"})
	plated := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "Mbak Tini", "plate": "B 4444 ZQT", "category": "keluarga"})
	bGuest := addRegular(t, f.ownerB.AccessToken, map[string]any{"guest_name": "Tamu Rumah B"})
	parse := func(rec *httptest.ResponseRecorder) regVisit { return decode[regVisit](t, rec) }

	// The entry supplies the house, name and category; the guard sends only its id.
	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID})
	assertStatus(t, rec, http.StatusCreated, "check-in from a regular guest")
	v := parse(rec)
	if v.House.ID != f.houseA || v.GuestName == nil || *v.GuestName != "Mas Budi Sopir" || v.Category != "tukang" || v.Status != "inside" {
		t.Fatalf("visit = %+v, want house A, the entry's name and category, inside", v)
	}
	if v.OwnerDecision.Status != "approved" || v.OwnerDecision.Source == nil || *v.OwnerDecision.Source != "regular_guest" || v.OwnerDecision.DecidedAt != nil {
		t.Fatalf("owner decision = %+v, want approved by a regular guest with no decision time", v.OwnerDecision)
	}
	var linked *string
	var approved bool
	if err := owner.QueryRow(context.Background(), `SELECT regular_guest_id::text, regular_guest_approved FROM visits WHERE id = $1`, v.ID).Scan(&linked, &approved); err != nil ||
		linked == nil || *linked != driver.ID || !approved {
		t.Fatalf("visit link = %v approved=%v (%v), want the entry and true", linked, approved, err)
	}
	// No arrival email: the owner approved this guest in advance. An ordinary guest still queues one.
	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE visit_id = $1`, v.ID); n != 0 {
		t.Fatalf("a regular guest queued %d emails, want 0", n)
	}
	ordinary := parse(do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"house_id": f.houseA, "guest_name": "Tamu Biasa RK", "category": "tamu"}))
	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE visit_id = $1`, ordinary.ID); n != 1 {
		t.Fatalf("an ordinary guest queued %d emails, want 1 (so the test shows the rule is specific)", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'regular_guest_used' AND target = $1 AND user_id = $2`, "regular:"+driver.ID, f.guard.User.ID); n != 1 {
		t.Fatalf("regular_guest_used audit rows = %d, want 1", n)
	}

	// Approved shows in the filters, not under pending; the owner may still reject, once.
	approvedList := decode[visitList](t, do(t, "GET", "/v1/visits?decision=approved&house_id="+f.houseA, f.guard.AccessToken, nil))
	if !slices.Contains(ids(approvedList), v.ID) {
		t.Fatalf("decision=approved misses the regular guest's visit: %v", ids(approvedList))
	}
	pendingList := decode[visitList](t, do(t, "GET", "/v1/visits?decision=pending&house_id="+f.houseA, f.guard.AccessToken, nil))
	if slices.Contains(ids(pendingList), v.ID) {
		t.Fatal("decision=pending returns a regular guest's visit")
	}
	rej := do(t, "POST", "/v1/visits/"+v.ID+"/decision", f.ownerA.AccessToken, map[string]any{"decision": "rejected"})
	assertStatus(t, rej, http.StatusOK, "owner rejects a regular guest's visit")
	if got := parse(rej); got.OwnerDecision.Status != "rejected" || got.OwnerDecision.Source == nil || *got.OwnerDecision.Source != "owner" {
		t.Fatalf("after rejecting: %+v", got.OwnerDecision)
	}
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/decision", f.ownerA.AccessToken, map[string]any{"decision": "approved"}), http.StatusConflict, "second change")

	// What the guard sends wins over the entry's defaults.
	named := parse(do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID, "guest_name": "Pengganti Sopir", "category": "keluarga"}))
	if named.GuestName == nil || *named.GuestName != "Pengganti Sopir" || named.Category != "keluarga" {
		t.Fatalf("explicit values lost: %+v", named)
	}

	// A plate from the entry still needs its vehicle photo, like any plate.
	rec = do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": plated.ID})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "plate from the entry without a photo")
	if !strings.Contains(rec.Body.String(), "vehicle_photo") {
		t.Fatalf("error should name vehicle_photo: %s", rec.Body)
	}
	withPhoto := parse(regularCheckIn(t, f.guard.AccessToken, map[string]string{"regular_guest_id": plated.ID}, true))
	if withPhoto.VehiclePlate == nil || *withPhoto.VehiclePlate != "B 4444 ZQT" || withPhoto.Category != "keluarga" {
		t.Fatalf("visit with a photo = %+v, want the entry's plate and category", withPhoto)
	}

	// Refusals create nothing: another house's entry with this house, an unknown id, an inactive entry,
	// and two sources of defaults at once.
	before := count(t, `SELECT count(*) FROM visits`)
	rec = do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID, "house_id": f.houseB})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "entry of another house")
	if !strings.Contains(rec.Body.String(), "house_id") {
		t.Fatalf("error should name house_id: %s", rec.Body)
	}
	rec = do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": uuid.NewString()})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "unknown entry")
	if !strings.Contains(rec.Body.String(), "regular_guest_id") {
		t.Fatalf("error should name regular_guest_id: %s", rec.Body)
	}
	assertStatus(t, do(t, "PATCH", "/v1/regular-guests/"+bGuest.ID, f.ownerB.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate B's guest")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": bGuest.ID}), http.StatusUnprocessableEntity, "inactive entry")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID, "pre_registration_code": "ABCD2345"}),
		http.StatusUnprocessableEntity, "regular guest and pre-registration code together")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": "not-a-uuid"}), http.StatusBadRequest, "malformed id")
	if after := count(t, `SELECT count(*) FROM visits`); after != before {
		t.Fatalf("a refused check-in created a visit (%d -> %d)", before, after)
	}

	// A retry of the same check-in answers 200 and audits nothing twice.
	key := uuid.NewString()
	first := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID, "client_request_id": key})
	assertStatus(t, first, http.StatusCreated, "keyed check-in")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"regular_guest_id": driver.ID, "client_request_id": key}), http.StatusOK, "replay")
	if n := count(t, `SELECT count(*) FROM visits WHERE client_request_id = $1`, key); n != 1 {
		t.Fatalf("%d visits for one key, want 1", n)
	}

	// Deleting the entry keeps the visits and, importantly, their approval: a guest who is still inside
	// must not drop back to "waiting for an answer" because the owner tidied the list.
	assertStatus(t, do(t, "DELETE", "/v1/regular-guests/"+plated.ID, f.ownerA.AccessToken, nil), http.StatusNoContent, "delete the entry")
	got := parse(do(t, "GET", "/v1/visits/"+withPhoto.ID, f.guard.AccessToken, nil))
	if got.OwnerDecision.Status != "approved" || got.OwnerDecision.Source == nil || *got.OwnerDecision.Source != "regular_guest" {
		t.Fatalf("after deleting the entry the visit reads %+v, want it still approved by a regular guest", got.OwnerDecision)
	}
	if err := owner.QueryRow(context.Background(), `SELECT regular_guest_id::text FROM visits WHERE id = $1`, withPhoto.ID).Scan(&linked); err != nil || linked != nil {
		t.Fatalf("the link after deleting the entry = %v (%v), want NULL", linked, err)
	}
	stillPending := decode[visitList](t, do(t, "GET", "/v1/visits?decision=pending&status=inside&house_id="+f.houseA, f.guard.AccessToken, nil))
	if slices.Contains(ids(stillPending), withPhoto.ID) {
		t.Fatal("a regular guest's visit became pending after its entry was deleted")
	}
}

// The database enforces the same rules whatever the service does.
func TestRegularGuestsRowLevelSecurity(t *testing.T) {
	f := newRegFixture(t, "RR")
	a := addRegular(t, f.ownerA.AccessToken, map[string]any{"guest_name": "Milik A"})
	ctx := context.Background()
	seen := func(tx pgx.Tx) int {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM regular_guests WHERE id = $1`, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	asRole(t, "owner", f.houseB, func(tx pgx.Tx) {
		if n := seen(tx); n != 0 {
			t.Fatalf("owner B reads A's entry through RLS (%d)", n)
		}
		if tag, err := tx.Exec(ctx, `UPDATE regular_guests SET guest_name = 'x' WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("owner B update of A's entry: affected=%d err=%v, want 0", tag.RowsAffected(), err)
		}
		if tag, err := tx.Exec(ctx, `DELETE FROM regular_guests WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("owner B delete of A's entry: affected=%d err=%v, want 0", tag.RowsAffected(), err)
		}
	})
	asRole(t, "owner", f.houseB, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO regular_guests (house_id, guest_name, created_by) VALUES ($1, 'x', $2)`, f.houseA, f.ownerB.User.ID); err == nil {
			t.Fatal("owner B inserted an entry for house A through RLS")
		}
	})
	asRole(t, "owner", f.houseA, func(tx pgx.Tx) {
		if n := seen(tx); n != 1 {
			t.Fatalf("owner A cannot read their own entry (%d)", n)
		}
		// An owner cannot move an entry to another house either.
		if _, err := tx.Exec(ctx, `UPDATE regular_guests SET house_id = $2 WHERE id = $1`, a.ID, f.houseB); err == nil {
			t.Fatal("owner A moved an entry to house B through RLS")
		}
	})
	for _, role := range []string{"guard", "admin"} {
		asRole(t, role, nil, func(tx pgx.Tx) {
			if n := seen(tx); n != 1 {
				t.Fatalf("%s cannot read the entry (%d)", role, n)
			}
			if tag, err := tx.Exec(ctx, `UPDATE regular_guests SET guest_name = 'x' WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s update: affected=%d err=%v, want 0", role, tag.RowsAffected(), err)
			}
			if tag, err := tx.Exec(ctx, `DELETE FROM regular_guests WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s delete: affected=%d err=%v, want 0", role, tag.RowsAffected(), err)
			}
		})
		asRole(t, role, nil, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `INSERT INTO regular_guests (house_id, guest_name, created_by) VALUES ($1, 'x', $2)`, f.houseA, f.ownerA.User.ID); err == nil {
				t.Fatalf("%s inserted an entry through RLS", role)
			}
		})
	}
}
