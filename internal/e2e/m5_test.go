package e2e

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ownerFixture builds two houses with an owner each, and visits for both.
type ownerFixture struct {
	houseA, houseB        string
	ownerA, ownerB, guard tokenPair
	visitsA               []visitBody // [0] has a vehicle photo, [1] does not
	visitB                visitBody   // has a vehicle photo
}

func newOwnerFixture(t *testing.T, tag string) ownerFixture {
	t.Helper()
	f := ownerFixture{houseA: newHouse(t, tag, "A", true), houseB: newHouse(t, tag, "B", true)}
	em := strings.ToLower(tag) // the API lower-cases emails; seeded rows must match
	seed(t, "Owner "+tag+"A", "owner-"+em+"a@example.test", "owner", f.houseA)
	seed(t, "Owner "+tag+"B", "owner-"+em+"b@example.test", "owner", f.houseB)
	seed(t, "Guard "+tag, "guard-"+em+"@example.test", "guard", nil)
	f.ownerA, f.ownerB = login(t, "owner-"+em+"a@example.test"), login(t, "owner-"+em+"b@example.test")
	f.guard = login(t, "guard-"+em+"@example.test")

	checkIn := func(house, name string, photo bool) visitBody {
		fields := map[string]string{"house_id": house, "guest_name": name, "category": "tamu"}
		files := map[string][]byte{}
		if photo {
			fields["vehicle_plate"] = "B 1 " + tag
			files["vehicle_photo"] = testJPEG(t, 60, 40, "")
		}
		rec := doForm(t, "/v1/visits", f.guard.AccessToken, fields, files)
		assertStatus(t, rec, http.StatusCreated, "fixture check-in")
		return decode[visitBody](t, rec)
	}
	f.visitsA = []visitBody{checkIn(f.houseA, "Tamu A1", true), checkIn(f.houseA, "Tamu A2", false)}
	f.visitB = checkIn(f.houseB, "Tamu B1", true)
	return f
}

func ids(list visitList) []string {
	out := make([]string, len(list.Data))
	for i, v := range list.Data {
		out[i] = v.ID
	}
	return out
}

// M5/M6: an owner reads their own house and nothing else, by list, by direct link, and by photo.
func TestOwnerSeesOnlyOwnHouse(t *testing.T) {
	f := newOwnerFixture(t, "OW")

	listA := decode[visitList](t, do(t, "GET", "/v1/visits", f.ownerA.AccessToken, nil))
	if len(listA.Data) != 2 {
		t.Fatalf("owner A sees %d visits, want 2: %v", len(listA.Data), ids(listA))
	}
	for _, v := range listA.Data {
		if v.House.ID != f.houseA {
			t.Fatalf("owner A sees a visit of house %s", v.House.ID)
		}
	}
	listB := decode[visitList](t, do(t, "GET", "/v1/visits", f.ownerB.AccessToken, nil))
	if len(listB.Data) != 1 || listB.Data[0].ID != f.visitB.ID {
		t.Fatalf("owner B sees %v, want only %s", ids(listB), f.visitB.ID)
	}

	// Naming another house in the filter returns nothing (and does not error, which would confirm it exists).
	other := decode[visitList](t, do(t, "GET", "/v1/visits?house_id="+f.houseB, f.ownerA.AccessToken, nil))
	if len(other.Data) != 0 || other.NextCursor != nil {
		t.Fatalf("owner A filtering by house B got %v", ids(other))
	}
	own := decode[visitList](t, do(t, "GET", "/v1/visits?house_id="+f.houseA, f.ownerA.AccessToken, nil))
	if len(own.Data) != 2 {
		t.Fatalf("owner A filtering by own house got %d, want 2", len(own.Data))
	}

	// Direct links: another house's visit and photo look exactly like missing ones (404, not 403).
	missing := do(t, "GET", "/v1/visits/"+uuid.NewString(), f.ownerA.AccessToken, nil)
	theirs := do(t, "GET", "/v1/visits/"+f.visitB.ID, f.ownerA.AccessToken, nil)
	assertStatus(t, theirs, http.StatusNotFound, "owner A, house B's visit")
	if theirs.Body.String() != missing.Body.String() {
		t.Fatalf("response differs from a missing visit:\n%s\nvs\n%s", theirs.Body, missing.Body)
	}
	assertStatus(t, do(t, "GET", *f.visitB.photoURL(), f.ownerA.AccessToken, nil), http.StatusNotFound, "owner A, house B's photo")
	assertStatus(t, do(t, "GET", "/v1/visits/"+f.visitsA[0].ID, f.ownerA.AccessToken, nil), http.StatusOK, "owner A, own visit")
	assertStatus(t, do(t, "GET", *f.visitsA[0].photoURL(), f.ownerA.AccessToken, nil), http.StatusOK, "owner A, own photo")

	// Owners cannot change the gate log, even for their own house.
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitsA[0].ID+"/checkout", f.ownerA.AccessToken, nil), http.StatusForbidden, "owner check-out")
	assertStatus(t, do(t, "POST", "/v1/visits", f.ownerA.AccessToken,
		map[string]any{"house_id": f.houseA, "guest_name": "X", "category": "tamu"}), http.StatusForbidden, "owner check-in")
	// ...nor read people or houses.
	assertStatus(t, do(t, "GET", "/v1/users", f.ownerA.AccessToken, nil), http.StatusForbidden, "owner lists users")
	assertStatus(t, do(t, "GET", "/v1/houses", f.ownerA.AccessToken, nil), http.StatusForbidden, "owner lists houses")

	// Status filter and paging stay inside the house.
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitsA[0].ID+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "guard check-out")
	inside := decode[visitList](t, do(t, "GET", "/v1/visits?status=inside", f.ownerA.AccessToken, nil))
	if len(inside.Data) != 1 || inside.Data[0].ID != f.visitsA[1].ID {
		t.Fatalf("owner A inside = %v", ids(inside))
	}
	var paged []string
	cursor := ""
	for i := 0; i < 5; i++ {
		path := "/v1/visits?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decode[visitList](t, do(t, "GET", path, f.ownerA.AccessToken, nil))
		paged = append(paged, ids(page)...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(paged) != 2 {
		t.Fatalf("paging as owner A returned %v, want its 2 visits", paged)
	}

	// Guards and admins still see everything.
	all := decode[visitList](t, do(t, "GET", "/v1/visits?limit=200", f.guard.AccessToken, nil))
	seen := map[string]bool{}
	for _, v := range all.Data {
		seen[v.ID] = true
	}
	if !seen[f.visitB.ID] || !seen[f.visitsA[0].ID] {
		t.Fatal("guard no longer sees all houses")
	}
}

func (v visitBody) photoURL() *string {
	u := "/v1/visits/" + v.ID + "/photos/vehicle"
	return &u
}

// Access follows the account's current state, not what the token said when it was issued.
func TestAccessChangesTakeEffectImmediately(t *testing.T) {
	f := newOwnerFixture(t, "RV")
	seed(t, "Admin RV", "admin-rv@example.test", "admin", nil)
	admin := login(t, "admin-rv@example.test")

	// An owner moved to another house loses the old house right away, with the same token.
	assertStatus(t, do(t, "GET", "/v1/visits/"+f.visitsA[0].ID, f.ownerA.AccessToken, nil), http.StatusOK, "before the move")
	assertStatus(t, do(t, "PATCH", "/v1/users/"+f.ownerA.User.ID, admin.AccessToken, map[string]any{"house_id": f.houseB}), http.StatusOK, "move owner A to house B")
	assertStatus(t, do(t, "GET", "/v1/visits/"+f.visitsA[0].ID, f.ownerA.AccessToken, nil), http.StatusNotFound, "old house after the move")
	moved := decode[visitList](t, do(t, "GET", "/v1/visits", f.ownerA.AccessToken, nil))
	if len(moved.Data) != 1 || moved.Data[0].ID != f.visitB.ID {
		t.Fatalf("moved owner sees %v, want only house B's visit", ids(moved))
	}

	// A deactivated guard is locked out immediately, not when the access token expires.
	assertStatus(t, do(t, "GET", "/v1/visits", f.guard.AccessToken, nil), http.StatusOK, "guard before deactivation")
	assertStatus(t, do(t, "PATCH", "/v1/users/"+f.guard.User.ID, admin.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate guard")
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/visits"}, {"GET", "/v1/auth/me"}, {"POST", "/v1/visits/" + f.visitB.ID + "/checkout"},
	} {
		assertStatus(t, do(t, req.method, req.path, f.guard.AccessToken, nil), http.StatusUnauthorized, "deactivated guard "+req.method+" "+req.path)
	}

	// Reactivating restores access with the same token.
	assertStatus(t, do(t, "PATCH", "/v1/users/"+f.guard.User.ID, admin.AccessToken, map[string]any{"active": true}), http.StatusOK, "reactivate guard")
	assertStatus(t, do(t, "GET", "/v1/visits", f.guard.AccessToken, nil), http.StatusOK, "guard after reactivation")
}

func TestChangePassword(t *testing.T) {
	seed(t, "Guard PW", "guard-pw@example.test", "guard", nil)
	seed(t, "Guard PW2", "guard-pw2@example.test", "guard", nil)
	me, other := login(t, "guard-pw@example.test"), login(t, "guard-pw2@example.test")
	const newPass = "A Brand New Pass 7"
	change := func(bearer, current, next string) int {
		return do(t, "POST", "/v1/auth/password", bearer, map[string]string{"current_password": current, "new_password": next}).Code
	}

	assertStatus(t, do(t, "POST", "/v1/auth/password", "", map[string]string{"current_password": testPassword, "new_password": newPass}),
		http.StatusUnauthorized, "anonymous")
	for name, c := range map[string][2]string{
		"wrong current": {"not the password", newPass},
		"too short":     {testPassword, "short"},
		"same as old":   {testPassword, testPassword},
		"empty current": {"", newPass},
	} {
		if code := change(me.AccessToken, c[0], c[1]); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", name, code)
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'password_changed' AND target = $1`, "user:"+me.User.ID); n != 0 {
		t.Fatalf("failed attempts were audited as changes: %d", n)
	}

	if code := change(me.AccessToken, testPassword, newPass); code != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", code)
	}
	assertStatus(t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "guard-pw@example.test", "password": testPassword}), http.StatusUnauthorized, "old password")
	assertStatus(t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "guard-pw@example.test", "password": newPass}), http.StatusOK, "new password")
	assertStatus(t, do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": me.RefreshToken}), http.StatusUnauthorized, "old session ended")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'password_changed' AND target = $1`, "user:"+me.User.ID); n != 1 {
		t.Fatalf("password_changed audit rows = %d, want 1", n)
	}
	// Someone else's session is untouched.
	assertStatus(t, do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": other.RefreshToken}), http.StatusOK, "other user's session")

	// Guessing the current password is rate limited.
	var last int
	for i := 0; i < 8; i++ {
		last = change(other.AccessToken, "wrong guess "+uuid.NewString()[:6], newPass)
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after repeated wrong guesses status = %d, want 429", last)
	}
}
