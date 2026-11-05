package e2e

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type watchEntry struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Note   string `json:"note"`
	Active bool   `json:"active"`
}

type watchList struct {
	Data       []watchEntry `json:"data"`
	NextCursor *string      `json:"next_cursor"`
}

// watchedVisit is a visit as the guard sees it, with the watchlist fields.
type watchedVisit struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	WatchlistHit  *bool   `json:"watchlist_hit"`
	WatchlistNote *string `json:"watchlist_note"`
}

type watchFixture struct {
	house               string
	admin, guard, owner tokenPair
}

func newWatchFixture(t *testing.T, tag string) watchFixture {
	t.Helper()
	em := strings.ToLower(tag)
	f := watchFixture{house: newHouse(t, "W"+tag, "1", true)}
	seed(t, "Admin "+tag, "admin-w-"+em+"@example.test", "admin", nil)
	seed(t, "Guard "+tag, "guard-w-"+em+"@example.test", "guard", nil)
	seed(t, "Owner "+tag, "owner-w-"+em+"@example.test", "owner", f.house)
	f.admin, f.guard, f.owner = login(t, "admin-w-"+em+"@example.test"), login(t, "guard-w-"+em+"@example.test"), login(t, "owner-w-"+em+"@example.test")
	return f
}

func (f watchFixture) add(t *testing.T, kind, value, note string) watchEntry {
	t.Helper()
	rec := do(t, "POST", "/v1/watchlist", f.admin.AccessToken, map[string]any{"kind": kind, "value": value, "note": note})
	assertStatus(t, rec, http.StatusCreated, "add watchlist entry "+value)
	return decode[watchEntry](t, rec)
}

// checkIn records a guest with a vehicle photo (so a plate is accepted) and returns the reply.
func (f watchFixture) checkIn(t *testing.T, fields map[string]string) (*watchedVisit, int) {
	t.Helper()
	fields["house_id"] = f.house
	if fields["category"] == "" {
		fields["category"] = "tamu"
	}
	files := map[string][]byte{}
	// A plate needs its vehicle photo, whether typed or taken from a pre-registration.
	if (fields["vehicle_plate"] != "" || fields["pre_registration_code"] != "") && fields["category"] != "kurir" && fields["category"] != "ojol" {
		files["vehicle_photo"] = testJPEG(t, 60, 40, "")
	}
	rec := doForm(t, "/v1/visits", f.guard.AccessToken, fields, files)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("check-in %v: status %d: %s", fields, rec.Code, rec.Body)
	}
	v := decode[watchedVisit](t, rec)
	return &v, rec.Code
}

func hit(v *watchedVisit) bool { return v.WatchlistHit != nil && *v.WatchlistHit }

// C1: only an admin manages the list, and values are stored normalized.
func TestWatchlistAdminManagement(t *testing.T) {
	f := newWatchFixture(t, "WA")

	plate := f.add(t, "plate", " b  1234  zqa ", "Hubungi pengurus")
	if plate.Value != "B1234ZQA" || plate.Kind != "plate" || !plate.Active || plate.Note != "Hubungi pengurus" {
		t.Fatalf("plate entry = %+v, want the plate upper-cased without spaces", plate)
	}
	name := f.add(t, "name", "  ZQ   Orang   Bermasalah ", "")
	if name.Value != "zq orang bermasalah" {
		t.Fatalf("name entry value = %q, want lower case with single spaces", name.Value)
	}

	// An active duplicate is refused, however it is written.
	for _, dup := range []map[string]any{
		{"kind": "plate", "value": "B1234ZQA"}, {"kind": "plate", "value": "b 1234 zqa"},
		{"kind": "name", "value": "zq orang bermasalah"}, {"kind": "name", "value": "Zq  ORANG bermasalah"},
	} {
		assertStatus(t, do(t, "POST", "/v1/watchlist", f.admin.AccessToken, dup), http.StatusConflict, "duplicate "+dup["value"].(string))
	}

	// Validation.
	for name, body := range map[string]map[string]any{
		"bad kind":          {"kind": "phone", "value": "08123"},
		"missing kind":      {"value": "B1234"},
		"plate with symbol": {"kind": "plate", "value": "B-1234"},
		"plate too short":   {"kind": "plate", "value": "B"},
		"plate too long":    {"kind": "plate", "value": "B1234ABCDEFGHI"},
		"name too short":    {"kind": "name", "value": "a"},
		"name too long":     {"kind": "name", "value": strings.Repeat("a", 101)},
		"note too long":     {"kind": "name", "value": "zq catatan panjang", "note": strings.Repeat("n", 201)},
		"empty":             {},
	} {
		assertStatus(t, do(t, "POST", "/v1/watchlist", f.admin.AccessToken, body), http.StatusUnprocessableEntity, name)
	}

	// List: newest first, filters, paging.
	all := decode[watchList](t, do(t, "GET", "/v1/watchlist?limit=1", f.admin.AccessToken, nil))
	if len(all.Data) != 1 || all.NextCursor == nil || all.Data[0].ID != name.ID {
		t.Fatalf("page 1 = %+v, want the newest entry and a cursor", all)
	}
	page2 := decode[watchList](t, do(t, "GET", "/v1/watchlist?limit=1&cursor="+*all.NextCursor, f.admin.AccessToken, nil))
	if len(page2.Data) == 0 || page2.Data[0].ID == name.ID {
		t.Fatalf("page 2 repeats the first entry: %+v", page2)
	}
	plates := decode[watchList](t, do(t, "GET", "/v1/watchlist?kind=plate&limit=200", f.admin.AccessToken, nil))
	for _, e := range plates.Data {
		if e.Kind != "plate" {
			t.Fatalf("kind=plate returned %+v", e)
		}
	}
	assertStatus(t, do(t, "GET", "/v1/watchlist?kind=phone", f.admin.AccessToken, nil), http.StatusUnprocessableEntity, "bad kind filter")
	assertStatus(t, do(t, "GET", "/v1/watchlist?active=maybe", f.admin.AccessToken, nil), http.StatusUnprocessableEntity, "bad active filter")

	// Update the note, deactivate, add the same plate again, and refuse to reactivate over an active one.
	up := decode[watchEntry](t, do(t, "PATCH", "/v1/watchlist/"+plate.ID, f.admin.AccessToken, map[string]any{"note": "Catatan baru"}))
	if up.Note != "Catatan baru" || !up.Active || up.Value != "B1234ZQA" {
		t.Fatalf("after a note change: %+v", up)
	}
	off := decode[watchEntry](t, do(t, "PATCH", "/v1/watchlist/"+plate.ID, f.admin.AccessToken, map[string]any{"active": false}))
	if off.Active || off.Note != "Catatan baru" {
		t.Fatalf("after deactivating: %+v (the note must stay)", off)
	}
	again := f.add(t, "plate", "B1234ZQA", "")
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+plate.ID, f.admin.AccessToken, map[string]any{"active": true}), http.StatusConflict, "reactivate over an active duplicate")
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+plate.ID, f.admin.AccessToken, map[string]any{}), http.StatusUnprocessableEntity, "empty update")
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+uuid.NewString(), f.admin.AccessToken, map[string]any{"note": "x"}), http.StatusNotFound, "update an unknown entry")
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/not-a-uuid", f.admin.AccessToken, map[string]any{"note": "x"}), http.StatusNotFound, "update a malformed id")

	// Delete for good, then again.
	assertStatus(t, do(t, "DELETE", "/v1/watchlist/"+again.ID, f.admin.AccessToken, nil), http.StatusNoContent, "delete")
	assertStatus(t, do(t, "DELETE", "/v1/watchlist/"+again.ID, f.admin.AccessToken, nil), http.StatusNotFound, "delete again")
	if n := count(t, `SELECT count(*) FROM watchlist_entries WHERE id = $1`, again.ID); n != 0 {
		t.Fatal("a deleted entry is still in the table")
	}

	// Only admins, on every route.
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "owner": f.owner.AccessToken} {
		assertStatus(t, do(t, "GET", "/v1/watchlist", tok, nil), http.StatusForbidden, name+" list")
		assertStatus(t, do(t, "POST", "/v1/watchlist", tok, map[string]any{"kind": "plate", "value": "B9999ZQ"}), http.StatusForbidden, name+" create")
		assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+name2id(plate), tok, map[string]any{"note": "x"}), http.StatusForbidden, name+" update")
		assertStatus(t, do(t, "DELETE", "/v1/watchlist/"+name2id(plate), tok, nil), http.StatusForbidden, name+" delete")
	}
	assertStatus(t, do(t, "GET", "/v1/watchlist", "", nil), http.StatusUnauthorized, "anonymous list")

	// Audit: every action is on record against the entry's id, never its plate, name or note.
	for action, want := range map[string]int{"watchlist_created": 3, "watchlist_updated": 2, "watchlist_deleted": 1} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = $1 AND user_id = $2`, action, f.admin.User.ID); n != want {
			t.Errorf("%s audit rows = %d, want %d", action, n, want)
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'watchlist%' AND target NOT LIKE 'watchlist:%'`); n != 0 {
		t.Fatalf("%d watchlist audit rows have an unexpected target", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'watchlist%' AND (target ILIKE '%zq%' OR target ILIKE '%1234%' OR target ILIKE '%Hubungi%' OR target ILIKE '%Catatan%')`); n != 0 {
		t.Fatalf("%d watchlist audit rows contain a plate, a name or a note", n)
	}
}

func name2id(e watchEntry) string { return e.ID }

// C1: a match is a flag for the guard, on every path a guest can be recorded, and never a block.
func TestWatchlistHitAtCheckIn(t *testing.T) {
	f := newWatchFixture(t, "WH")
	f.add(t, "plate", "B 8888 ZQW", "Hubungi pengurus")
	f.add(t, "name", "zq buron", "")

	// A plate written differently still matches, and the guest is recorded all the same.
	v, code := f.checkIn(t, map[string]string{"guest_name": "Tamu Biasa", "vehicle_plate": "b8888zqw"})
	if code != http.StatusCreated || !hit(v) {
		t.Fatalf("plate match: status %d hit=%v, want a recorded check-in with a hit", code, v.WatchlistHit)
	}
	if v.WatchlistNote == nil || *v.WatchlistNote != "Hubungi pengurus" {
		t.Fatalf("watchlist_note = %v, want the admin's instruction in the creating response", v.WatchlistNote)
	}
	if v.Status != "inside" {
		t.Fatalf("a hit must not block: status = %q", v.Status)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'watchlist_hit' AND target = $1`, "visit:"+v.ID); n != 1 {
		t.Fatalf("watchlist_hit audit rows = %d, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visit_checked_in' AND target = $1`, "visit:"+v.ID); n != 1 {
		t.Fatal("the check-in itself was not audited")
	}
	var stored bool
	if err := owner.QueryRow(context.Background(), `SELECT watchlist_hit FROM visits WHERE id = $1`, v.ID).Scan(&stored); err != nil || !stored {
		t.Fatalf("visits.watchlist_hit = %v (%v), want true", stored, err)
	}

	// A name matches regardless of case and spacing; an entry without a note adds no note.
	named, _ := f.checkIn(t, map[string]string{"guest_name": "  ZQ   Buron "})
	if !hit(named) || named.WatchlistNote != nil {
		t.Fatalf("name match: hit=%v note=%v, want a hit without a note", named.WatchlistHit, named.WatchlistNote)
	}

	// No match: the flag is false, there is no note and nothing is audited.
	miss, _ := f.checkIn(t, map[string]string{"guest_name": "Zq Aman", "vehicle_plate": "B 1 ZQAMAN"})
	if miss.WatchlistHit == nil || *miss.WatchlistHit || miss.WatchlistNote != nil {
		t.Fatalf("no match: hit=%v note=%v, want hit=false and no note", miss.WatchlistHit, miss.WatchlistNote)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'watchlist_hit' AND target = $1`, "visit:"+miss.ID); n != 0 {
		t.Fatal("a visit with no match was audited as a hit")
	}

	// The courier and ojol fast lane is checked too, and so is a plate that came from a pre-registration.
	fast, _ := f.checkIn(t, map[string]string{"category": "kurir", "vehicle_plate": "B 8888 ZQW"})
	if !hit(fast) {
		t.Fatal("a fast-lane check-in with a watched plate was not flagged")
	}
	pre := mustCreatePre(t, f.owner.AccessToken, "Zq Prereg Biasa", day(0), "B 8888 ZQW")
	viaCode, _ := f.checkIn(t, map[string]string{"pre_registration_code": pre.Code, "vehicle_plate": ""})
	if !hit(viaCode) {
		t.Fatal("a plate that came from a pre-registration was not checked against the watchlist")
	}

	// Later reads keep the flag but not the note (it is not stored on the visit).
	got := decode[watchedVisit](t, do(t, "GET", "/v1/visits/"+v.ID, f.guard.AccessToken, nil))
	if !hit(&got) || got.WatchlistNote != nil {
		t.Fatalf("a later read: hit=%v note=%v, want the flag without the note", got.WatchlistHit, got.WatchlistNote)
	}

	// A retry of the same check-in answers 200 and records nothing twice.
	key := uuid.NewString()
	first := f.checkInWithKey(t, key, "b8888zqw")
	if !hit(first) {
		t.Fatal("the keyed check-in was not flagged")
	}
	rec := doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{
		"house_id": f.house, "guest_name": "Tamu Kunci", "category": "tamu", "vehicle_plate": "b8888zqw", "client_request_id": key,
	}, map[string][]byte{"vehicle_photo": testJPEG(t, 60, 40, "")})
	assertStatus(t, rec, http.StatusOK, "replay")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'watchlist_hit' AND target = $1`, "visit:"+first.ID); n != 1 {
		t.Fatalf("a replay wrote %d watchlist_hit rows, want 1", n)
	}

	// The owner never sees the flag or the note, on a single visit or in a list.
	ownerView := decode[map[string]any](t, do(t, "GET", "/v1/visits/"+v.ID, f.owner.AccessToken, nil))
	for _, k := range []string{"watchlist_hit", "watchlist_note"} {
		if _, present := ownerView[k]; present {
			t.Fatalf("the owner's view of a visit contains %q", k)
		}
	}
	list := do(t, "GET", "/v1/visits?limit=200", f.owner.AccessToken, nil)
	assertStatus(t, list, http.StatusOK, "owner list")
	if strings.Contains(list.Body.String(), "watchlist") {
		t.Fatal("the owner's visit list mentions the watchlist")
	}

	// An inactive entry no longer matches, and neither does a deleted one.
	entries := decode[watchList](t, do(t, "GET", "/v1/watchlist?kind=name&limit=200", f.admin.AccessToken, nil))
	var nameEntry watchEntry
	for _, e := range entries.Data {
		if e.Value == "zq buron" {
			nameEntry = e
		}
	}
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+nameEntry.ID, f.admin.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate")
	if off, _ := f.checkIn(t, map[string]string{"guest_name": "Zq Buron"}); hit(off) {
		t.Fatal("an inactive entry still matches")
	}
	assertStatus(t, do(t, "PATCH", "/v1/watchlist/"+nameEntry.ID, f.admin.AccessToken, map[string]any{"active": true}), http.StatusOK, "reactivate")
	if on, _ := f.checkIn(t, map[string]string{"guest_name": "Zq Buron"}); !hit(on) {
		t.Fatal("a reactivated entry does not match")
	}
	assertStatus(t, do(t, "DELETE", "/v1/watchlist/"+nameEntry.ID, f.admin.AccessToken, nil), http.StatusNoContent, "delete")
	if gone, _ := f.checkIn(t, map[string]string{"guest_name": "Zq Buron"}); hit(gone) {
		t.Fatal("a deleted entry still matches")
	}

	// A guest who is only a partial match is not flagged: matching is exact on the normalized value.
	if part, _ := f.checkIn(t, map[string]string{"guest_name": "Zq Buron Besar"}); hit(part) {
		t.Fatal("a name that merely contains a watched name was flagged")
	}
}

func (f watchFixture) checkInWithKey(t *testing.T, key, plate string) *watchedVisit {
	t.Helper()
	rec := doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{
		"house_id": f.house, "guest_name": "Tamu Kunci", "category": "tamu", "vehicle_plate": plate, "client_request_id": key,
	}, map[string][]byte{"vehicle_photo": testJPEG(t, 60, 40, "")})
	assertStatus(t, rec, http.StatusCreated, "keyed check-in")
	v := decode[watchedVisit](t, rec)
	return &v
}

// The guard role cannot read the list even through the database, whatever the service does. It learns
// of a match only through the SECURITY DEFINER function, which returns one note and nothing else.
func TestWatchlistIsNotReadableByGuards(t *testing.T) {
	f := newWatchFixture(t, "WR")
	e := f.add(t, "plate", "B 7777 ZQR", "Hubungi pengurus")
	ctx := context.Background()

	for _, role := range []string{"guard", "owner"} {
		house := any(nil)
		if role == "owner" {
			house = f.house
		}
		asRole(t, role, house, func(tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM watchlist_entries`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s reads %d watchlist rows through RLS (err %v), want 0", role, n, err)
			}
			if tag, err := tx.Exec(ctx, `UPDATE watchlist_entries SET note = 'x' WHERE id = $1`, e.ID); err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s update: affected=%d err=%v, want 0", role, tag.RowsAffected(), err)
			}
			if tag, err := tx.Exec(ctx, `DELETE FROM watchlist_entries WHERE id = $1`, e.ID); err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s delete: affected=%d err=%v, want 0", role, tag.RowsAffected(), err)
			}
		})
		asRole(t, role, house, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `INSERT INTO watchlist_entries (kind, value, created_by) VALUES ('plate', 'B6666ZQR', $1)`, f.admin.User.ID); err == nil {
				t.Fatalf("%s inserted into the watchlist through RLS", role)
			}
		})
	}
	asRole(t, "admin", nil, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM watchlist_entries WHERE id = $1`, e.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("an admin cannot read the entry (n=%d err=%v)", n, err)
		}
	})

	// The function still answers for a guard, with the note only; no match is NULL.
	asRole(t, "guard", nil, func(tx pgx.Tx) {
		var note *string
		if err := tx.QueryRow(ctx, `SELECT watchlist_check('b 7777 zqr', NULL)`).Scan(&note); err != nil || note == nil || *note != "Hubungi pengurus" {
			t.Fatalf("watchlist_check as a guard = %v (%v), want the note", note, err)
		}
		if err := tx.QueryRow(ctx, `SELECT watchlist_check('B 1 NOPE', 'tidak terdaftar')`).Scan(&note); err != nil || note != nil {
			t.Fatalf("watchlist_check with no match = %v (%v), want NULL", note, err)
		}
	})

	// The function is callable by the app role only, not by everyone.
	if n := count(t, `SELECT count(*) FROM information_schema.routine_privileges WHERE routine_name = 'watchlist_check' AND grantee = 'PUBLIC'`); n != 0 {
		t.Fatalf("watchlist_check is executable by PUBLIC (%d grants)", n)
	}
	if n := count(t, `SELECT count(*) FROM information_schema.routine_privileges WHERE routine_name = 'watchlist_check' AND grantee = 'guestbook_app'`); n != 1 {
		t.Fatal("the app role cannot execute watchlist_check")
	}
	// It runs with its owner's rights and a pinned search path.
	var definer bool
	var cfg []string
	if err := owner.QueryRow(ctx, `SELECT prosecdef, coalesce(proconfig, '{}') FROM pg_proc WHERE proname = 'watchlist_check'`).Scan(&definer, &cfg); err != nil {
		t.Fatal(err)
	}
	if !definer || !slices.ContainsFunc(cfg, func(s string) bool { return strings.HasPrefix(s, "search_path=") }) {
		t.Fatalf("watchlist_check security definer=%v config=%v, want a definer function with a pinned search_path", definer, cfg)
	}
}
