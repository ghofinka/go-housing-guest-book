package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var wib = time.FixedZone("WIB", 7*3600)

// day returns today (WIB) shifted by n days, as YYYY-MM-DD.
func day(n int) string { return time.Now().In(wib).AddDate(0, 0, n).Format(time.DateOnly) }

type preBody struct {
	ID    string `json:"id"`
	House struct {
		ID string `json:"id"`
	} `json:"house"`
	GuestName    *string `json:"guest_name"`
	ExpectedDate string  `json:"expected_date"`
	Plate        *string `json:"plate"`
	Code         string  `json:"code"`
	Status       string  `json:"status"`
	Valid        bool    `json:"valid"`
	UsedAt       *string `json:"used_at"`
	Anonymized   bool    `json:"anonymized"`
}

type preList struct {
	Data       []preBody `json:"data"`
	NextCursor *string   `json:"next_cursor"`
}

type preFixture struct {
	houseA, houseB               string
	ownerA, ownerB, guard, admin tokenPair
}

func newPreFixture(t *testing.T, tag string) preFixture {
	t.Helper()
	em := strings.ToLower(tag)
	f := preFixture{houseA: newHouse(t, tag, "A", true), houseB: newHouse(t, tag, "B", true)}
	seed(t, "Owner "+tag+"A", "owner-"+em+"a@example.test", "owner", f.houseA)
	seed(t, "Owner "+tag+"B", "owner-"+em+"b@example.test", "owner", f.houseB)
	seed(t, "Guard "+tag, "guard-"+em+"@example.test", "guard", nil)
	seed(t, "Admin "+tag, "admin-"+em+"@example.test", "admin", nil)
	f.ownerA, f.ownerB = login(t, "owner-"+em+"a@example.test"), login(t, "owner-"+em+"b@example.test")
	f.guard, f.admin = login(t, "guard-"+em+"@example.test"), login(t, "admin-"+em+"@example.test")
	return f
}

func createPre(t *testing.T, token, name, date, plate string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"guest_name": name, "expected_date": date}
	if plate != "" {
		body["plate"] = plate
	}
	return do(t, "POST", "/v1/pre-registrations", token, body)
}

func mustCreatePre(t *testing.T, token, name, date, plate string) preBody {
	t.Helper()
	rec := createPre(t, token, name, date, plate)
	assertStatus(t, rec, http.StatusCreated, "create pre-registration "+name)
	return decode[preBody](t, rec)
}

func preStatus(t *testing.T, id string) (status string, used bool) {
	t.Helper()
	if err := owner.QueryRow(context.Background(),
		`SELECT status, used_at IS NOT NULL FROM pre_registrations WHERE id = $1`, id).Scan(&status, &used); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPreRegistrationCreate(t *testing.T) {
	f := newPreFixture(t, "PC")

	pre := mustCreatePre(t, f.ownerA.AccessToken, "  Zqtamu Create  ", day(0), "b 55 zqc")
	if len(pre.Code) != 8 || strings.Trim(pre.Code, "ABCDEFGHJKLMNPQRSTUVWXYZ23456789") != "" {
		t.Fatalf("code %q is not 8 characters from the unambiguous alphabet", pre.Code)
	}
	if pre.House.ID != f.houseA || pre.Status != "active" || !pre.Valid || pre.ExpectedDate != day(0) {
		t.Fatalf("unexpected pre-registration: %+v", pre)
	}
	if pre.GuestName == nil || *pre.GuestName != "Zqtamu Create" || pre.Plate == nil || *pre.Plate != "B 55 ZQC" {
		t.Fatalf("name/plate not trimmed and normalized: %+v", pre)
	}
	// A future day is allowed but not usable yet.
	if future := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Besok", day(1), ""); future.Valid || future.Status != "active" {
		t.Fatalf("tomorrow's pre-registration should be active but not valid yet: %+v", future)
	}

	for name, body := range map[string]map[string]any{
		"missing name":      {"expected_date": day(0)},
		"blank name":        {"guest_name": "   ", "expected_date": day(0)},
		"name too long":     {"guest_name": strings.Repeat("a", 101), "expected_date": day(0)},
		"missing date":      {"guest_name": "X"},
		"bad date":          {"guest_name": "X", "expected_date": "besok"},
		"past date":         {"guest_name": "X", "expected_date": day(-1)},
		"too far ahead":     {"guest_name": "X", "expected_date": day(31)},
		"plate with symbol": {"guest_name": "X", "expected_date": day(0), "plate": "B-1"},
	} {
		assertStatus(t, do(t, "POST", "/v1/pre-registrations", f.ownerA.AccessToken, body), http.StatusUnprocessableEntity, name)
	}
	assertStatus(t, do(t, "POST", "/v1/pre-registrations", f.ownerA.AccessToken, map[string]any{
		"guest_name": "X", "expected_date": day(30)}), http.StatusCreated, "30 days ahead is the limit")

	// Only owners create pre-registrations.
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "admin": f.admin.AccessToken} {
		assertStatus(t, createPre(t, tok, "X", day(0), ""), http.StatusForbidden, name+" create")
	}
	assertStatus(t, createPre(t, "", "X", day(0), ""), http.StatusUnauthorized, "anonymous create")
}

func TestPreRegistrationCheckInUsesCodeOnce(t *testing.T) {
	f := newPreFixture(t, "PU")
	photo := map[string][]byte{"vehicle_photo": testJPEG(t, 60, 40, "")}

	// Guard looks the code up first (any case, with a hyphen), then checks the guest in with it.
	pre := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Pakai", day(0), "b 55 zqp")
	typed := strings.ToLower(pre.Code[:4]) + "-" + pre.Code[4:]
	look := decode[preBody](t, do(t, "GET", "/v1/pre-registrations/by-code/"+typed, f.guard.AccessToken, nil))
	if look.ID != pre.ID || !look.Valid || look.GuestName == nil || *look.GuestName != "Zqtamu Pakai" {
		t.Fatalf("lookup = %+v", look)
	}

	rec := doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{"pre_registration_code": typed}, photo)
	assertStatus(t, rec, http.StatusCreated, "check-in with code")
	v := decode[fullVisit](t, rec)
	if v.House.ID != f.houseA || v.Category != "tamu" || v.GuestName == nil || *v.GuestName != "Zqtamu Pakai" ||
		v.VehiclePlate == nil || *v.VehiclePlate != "B 55 ZQP" {
		t.Fatalf("visit should take house, name and plate from the pre-registration: %+v", v)
	}
	var linked string
	if err := owner.QueryRow(context.Background(), `SELECT pre_registration_id::text FROM visits WHERE id = $1`, v.ID).Scan(&linked); err != nil || linked != pre.ID {
		t.Fatalf("visit.pre_registration_id = %q (%v), want %s", linked, err, pre.ID)
	}
	if st, used := preStatus(t, pre.ID); st != "used" || !used {
		t.Fatalf("after check-in: status %q used_at set=%v, want used", st, used)
	}
	if got := decode[preBody](t, do(t, "GET", "/v1/pre-registrations/"+pre.ID, f.ownerA.AccessToken, nil)); got.Status != "used" || got.Valid {
		t.Fatalf("owner sees %+v, want used and not valid", got)
	}

	// A code works once.
	visits0 := count(t, `SELECT count(*) FROM visits WHERE pre_registration_id = $1`, pre.ID)
	rec = doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{"pre_registration_code": pre.Code}, photo)
	assertStatus(t, rec, http.StatusUnprocessableEntity, "second use")
	if !strings.Contains(rec.Body.String(), "pre_registration_code") {
		t.Fatalf("error should name pre_registration_code: %s", rec.Body)
	}
	if n := count(t, `SELECT count(*) FROM visits WHERE pre_registration_id = $1`, pre.ID); n != visits0 {
		t.Fatalf("second use created a visit (%d -> %d)", visits0, n)
	}
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/"+pre.ID+"/cancel", f.ownerA.AccessToken, nil), http.StatusConflict, "cancel a used code")

	// The plate still needs its photo; a refused check-in leaves the code unused.
	withPlate := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Plat", day(0), "B 56 ZQP")
	rec = do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": withPlate.Code})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "plate from pre-registration without photo")
	if !strings.Contains(rec.Body.String(), "vehicle_photo") {
		t.Fatalf("error should name vehicle_photo: %s", rec.Body)
	}
	if st, _ := preStatus(t, withPlate.ID); st != "active" {
		t.Fatalf("a refused check-in must not use the code, status = %q", st)
	}

	// A retry of the same check-in (same client_request_id) returns the original visit.
	noPlate := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Retry", day(0), "")
	body := map[string]any{"pre_registration_code": noPlate.Code, "client_request_id": uuid.NewString()}
	first := do(t, "POST", "/v1/visits", f.guard.AccessToken, body)
	assertStatus(t, first, http.StatusCreated, "first attempt")
	retry := do(t, "POST", "/v1/visits", f.guard.AccessToken, body)
	assertStatus(t, retry, http.StatusOK, "retry of the same check-in")
	if decode[fullVisit](t, first).ID != decode[fullVisit](t, retry).ID {
		t.Fatal("retry returned a different visit")
	}

	// The code belongs to one house.
	other := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Rumah", day(0), "")
	rec = do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": other.Code, "house_id": f.houseB})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "code of another house")
	if !strings.Contains(rec.Body.String(), "house_id") {
		t.Fatalf("error should name house_id: %s", rec.Body)
	}
	if st, _ := preStatus(t, other.ID); st != "active" {
		t.Fatalf("house mismatch must not use the code, status = %q", st)
	}

	// Valid only on its day: yesterday's and tomorrow's codes are refused, and the lookup says why.
	for name, date := range map[string]string{"ZQKEMARN": day(-1), "ZQBESUKK": day(1)} {
		if _, err := owner.Exec(context.Background(),
			`INSERT INTO pre_registrations (house_id, guest_name, expected_date, code, expires_at, created_by)
			 VALUES ($1, 'Zq Salah Hari', $2::date, $3, now(), $4)`, f.houseA, date, name, f.ownerA.User.ID); err != nil {
			t.Fatal(err)
		}
		assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": name}),
			http.StatusUnprocessableEntity, "code "+name+" on the wrong day")
		if l := decode[preBody](t, do(t, "GET", "/v1/pre-registrations/by-code/"+name, f.guard.AccessToken, nil)); l.Valid || l.Status != "active" {
			t.Fatalf("lookup of %s = %+v, want active but not valid", name, l)
		}
	}

	// Unknown and malformed codes, and who may look up.
	assertStatus(t, do(t, "GET", "/v1/pre-registrations/by-code/ZQNTHERR", f.guard.AccessToken, nil), http.StatusNotFound, "unknown code")
	assertStatus(t, do(t, "GET", "/v1/pre-registrations/by-code/abc", f.guard.AccessToken, nil), http.StatusNotFound, "malformed code")
	assertStatus(t, do(t, "GET", "/v1/pre-registrations/by-code/"+pre.Code, f.ownerA.AccessToken, nil), http.StatusForbidden, "owner lookup by code")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": "ZQNTHERR"}),
		http.StatusUnprocessableEntity, "check-in with an unknown code")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": "short"}),
		http.StatusUnprocessableEntity, "check-in with a malformed code")

	// Audit: created, looked up and used are recorded by id; the target never carries name or plate.
	target := "prereg:" + pre.ID
	for _, action := range []string{"prereg_created", "prereg_viewed", "prereg_used"} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = $2`, target, action); n == 0 {
			t.Fatalf("no %s audit row for %s", action, target)
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'prereg%' AND (target ILIKE '%zqtamu%' OR target ILIKE '%zqp%')`); n != 0 {
		t.Fatalf("audit targets contain a name or plate (%d rows)", n)
	}
}

func TestPreRegistrationIsolation(t *testing.T) {
	f := newPreFixture(t, "PI")
	a := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu A", day(0), "")
	b := mustCreatePre(t, f.ownerB.AccessToken, "Zqtamu B", day(0), "")

	// By id, another house is indistinguishable from a missing record.
	missing := do(t, "GET", "/v1/pre-registrations/"+uuid.NewString(), f.ownerB.AccessToken, nil)
	cross := do(t, "GET", "/v1/pre-registrations/"+a.ID, f.ownerB.AccessToken, nil)
	assertStatus(t, cross, http.StatusNotFound, "owner B reads A's pre-registration")
	if cross.Body.String() != missing.Body.String() {
		t.Fatalf("cross-house 404 differs from a missing id:\n%s\n%s", cross.Body, missing.Body)
	}
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/"+a.ID+"/cancel", f.ownerB.AccessToken, nil), http.StatusNotFound, "owner B cancels A's")
	if st, _ := preStatus(t, a.ID); st != "active" {
		t.Fatalf("cross-house cancel changed the row, status = %q", st)
	}

	// Lists: an owner sees only their house; asking for another house matches nothing.
	listA := decode[preList](t, do(t, "GET", "/v1/pre-registrations", f.ownerA.AccessToken, nil))
	if len(listA.Data) != 1 || listA.Data[0].ID != a.ID {
		t.Fatalf("owner A sees %+v, want only %s", listA.Data, a.ID)
	}
	other := decode[preList](t, do(t, "GET", "/v1/pre-registrations?house_id="+f.houseB, f.ownerA.AccessToken, nil))
	if len(other.Data) != 0 {
		t.Fatalf("owner A asking for house B sees %d rows", len(other.Data))
	}
	adminList := decode[preList](t, do(t, "GET", "/v1/pre-registrations?limit=1", f.admin.AccessToken, nil))
	if len(adminList.Data) != 1 || adminList.NextCursor == nil {
		t.Fatalf("admin page 1 = %+v, want 1 row and a cursor", adminList)
	}
	byHouse := decode[preList](t, do(t, "GET", "/v1/pre-registrations?house_id="+f.houseB, f.admin.AccessToken, nil))
	if len(byHouse.Data) != 1 || byHouse.Data[0].ID != b.ID {
		t.Fatalf("admin filtered by house B sees %+v", byHouse.Data)
	}
	assertStatus(t, do(t, "GET", "/v1/pre-registrations", f.guard.AccessToken, nil), http.StatusForbidden, "guard list")
	assertStatus(t, do(t, "GET", "/v1/pre-registrations?status=bogus", f.ownerA.AccessToken, nil), http.StatusUnprocessableEntity, "bad status")

	// The database enforces it too, whatever the service does: row-level security on the app role.
	inRole := func(role string, house any, fn func(tx pgx.Tx)) { t.Helper(); asRole(t, role, house, fn) }
	ctx := context.Background()
	rows := func(tx pgx.Tx, id string) int {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pre_registrations WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	inRole("owner", f.houseB, func(tx pgx.Tx) {
		if n := rows(tx, a.ID); n != 0 {
			t.Fatalf("owner B can select A's row through RLS (%d)", n)
		}
		if tag, err := tx.Exec(ctx, `UPDATE pre_registrations SET status = 'cancelled' WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("owner B update of A's row: affected=%d err=%v, want 0", tag.RowsAffected(), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pre_registrations (house_id, guest_name, expected_date, code, expires_at, created_by)
			VALUES ($1, 'x', current_date, 'ZQRLSBAD', now(), $2)`, f.houseA, f.ownerB.User.ID); err == nil {
			t.Fatal("owner B inserted a pre-registration for house A through RLS")
		}
	})
	inRole("owner", f.houseA, func(tx pgx.Tx) {
		if n := rows(tx, a.ID); n != 1 {
			t.Fatalf("owner A cannot select their own row (%d)", n)
		}
	})
	for _, role := range []string{"guard", "admin", "owner"} {
		house := any(nil)
		if role == "owner" {
			house = f.houseA
		}
		inRole(role, house, func(tx pgx.Tx) {
			if tag, err := tx.Exec(ctx, `DELETE FROM pre_registrations WHERE id = $1`, a.ID); err != nil || tag.RowsAffected() != 0 {
				t.Fatalf("%s delete: affected=%d err=%v, want 0 (no DELETE policy)", role, tag.RowsAffected(), err)
			}
		})
	}
	inRole("guard", nil, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO pre_registrations (house_id, guest_name, expected_date, code, expires_at, created_by)
			VALUES ($1, 'x', current_date, 'ZQRLSGRD', now(), $2)`, f.houseA, f.guard.User.ID); err == nil {
			t.Fatal("a guard inserted a pre-registration through RLS")
		}
	})
}

func TestPreRegistrationCancel(t *testing.T) {
	f := newPreFixture(t, "PX")
	pre := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Batal", day(0), "B 57 ZQX")

	got := decode[preBody](t, do(t, "POST", "/v1/pre-registrations/"+pre.ID+"/cancel", f.ownerA.AccessToken, nil))
	if got.Status != "cancelled" || got.GuestName != nil || got.Plate != nil || !got.Anonymized || got.Valid {
		t.Fatalf("cancelled pre-registration = %+v, want cancelled with name and plate cleared", got)
	}
	var name, plate *string
	if err := owner.QueryRow(context.Background(), `SELECT guest_name, plate FROM pre_registrations WHERE id = $1`, pre.ID).Scan(&name, &plate); err != nil || name != nil || plate != nil {
		t.Fatalf("name/plate still stored after cancel: %v %v (%v)", name, plate, err)
	}

	// Idempotent: a second cancel is the same result and is audited once.
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/"+pre.ID+"/cancel", f.ownerA.AccessToken, nil), http.StatusOK, "cancel again")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'prereg_cancelled'`, "prereg:"+pre.ID); n != 1 {
		t.Fatalf("prereg_cancelled audit rows = %d, want 1", n)
	}

	// A cancelled code cannot be used, and the guard's lookup shows it.
	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": pre.Code})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "check-in with a cancelled code")
	if l := decode[preBody](t, do(t, "GET", "/v1/pre-registrations/by-code/"+pre.Code, f.guard.AccessToken, nil)); l.Status != "cancelled" || l.Valid {
		t.Fatalf("lookup = %+v, want cancelled and not valid", l)
	}
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/"+pre.ID+"/cancel", f.guard.AccessToken, nil), http.StatusForbidden, "guard cancel")
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/not-a-uuid/cancel", f.ownerA.AccessToken, nil), http.StatusNotFound, "bad id")
}

func TestPreRegistrationActiveLimit(t *testing.T) {
	f := newPreFixture(t, "PM")
	var first preBody
	for i := 0; i < 20; i++ {
		p := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Banyak", day(0), "")
		if i == 0 {
			first = p
		}
	}
	assertStatus(t, createPre(t, f.ownerA.AccessToken, "Zqtamu Ke-21", day(0), ""), http.StatusConflict, "21st active pre-registration")
	// Another house is unaffected, and cancelling frees a slot.
	mustCreatePre(t, f.ownerB.AccessToken, "Zqtamu Lain", day(0), "")
	assertStatus(t, do(t, "POST", "/v1/pre-registrations/"+first.ID+"/cancel", f.ownerA.AccessToken, nil), http.StatusOK, "cancel one")
	assertStatus(t, createPre(t, f.ownerA.AccessToken, "Zqtamu Lagi", day(0), ""), http.StatusCreated, "create after cancelling")
}

func TestPreRegistrationRetention(t *testing.T) {
	resetRetentionSettings(t)
	f := newPreFixture(t, "PR")
	run := func(query string) struct {
		DryRun            bool `json:"dry_run"`
		PreRegsAnonymized int  `json:"preregs_anonymized"`
	} {
		t.Helper()
		rec := do(t, "POST", "/v1/retention/run"+query, f.admin.AccessToken, nil)
		assertStatus(t, rec, http.StatusOK, "retention run "+query)
		return decode[struct {
			DryRun            bool `json:"dry_run"`
			PreRegsAnonymized int  `json:"preregs_anonymized"`
		}](t, rec)
	}
	run("") // clear anything old left by earlier tests so the counts below are exact

	insert := func(code string, ageDays int) string {
		var id string
		if err := owner.QueryRow(context.Background(),
			`INSERT INTO pre_registrations (house_id, guest_name, expected_date, plate, code, expires_at, created_by, created_at)
			 VALUES ($1, 'Zq Lama', current_date, 'B 9 ZQ', $2, now(), $3, now() - make_interval(days => $4)) RETURNING id`,
			f.houseA, code, f.ownerA.User.ID, ageDays).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, recent := insert("ZQLAMAAA", 100), insert("ZQBARUUU", 10)
	state := func(id string) (name, plate *string, anonymized bool) {
		if err := owner.QueryRow(context.Background(),
			`SELECT guest_name, plate, anonymized_at IS NOT NULL FROM pre_registrations WHERE id = $1`, id).Scan(&name, &plate, &anonymized); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Dry run reports what would happen and changes nothing.
	if dry := run("?dry_run=true"); !dry.DryRun || dry.PreRegsAnonymized != 1 {
		t.Fatalf("dry run = %+v, want 1 pre-registration", dry)
	}
	if name, _, anon := state(old); name == nil || anon {
		t.Fatal("a dry run changed the old pre-registration")
	}

	if res := run(""); res.PreRegsAnonymized != 1 {
		t.Fatalf("run = %+v, want 1 pre-registration anonymized", res)
	}
	if name, plate, anon := state(old); name != nil || plate != nil || !anon {
		t.Fatalf("old pre-registration still holds personal data: %v %v anonymized=%v", name, plate, anon)
	}
	if name, plate, anon := state(recent); name == nil || plate == nil || anon {
		t.Fatal("a recent pre-registration was anonymized")
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'retention_run' AND target LIKE '%preregs:1'`); n == 0 {
		t.Fatal("the retention_run audit row does not carry the pre-registration count")
	}
	if res := run(""); res.PreRegsAnonymized != 0 {
		t.Fatalf("second run = %+v, want nothing left to do", res)
	}
}
