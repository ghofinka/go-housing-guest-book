package e2e

import (
	"net/http"
	"strings"
	"testing"
)

type houseBody struct {
	ID     string `json:"id"`
	Block  string `json:"block"`
	Number string `json:"number"`
	Active bool   `json:"active"`
}

type userBody struct {
	ID      string  `json:"id"`
	Email   string  `json:"email"`
	Role    string  `json:"role"`
	HouseID *string `json:"house_id"`
	Active  bool    `json:"active"`
}

func assertStatus(t *testing.T, rec interface{ Result() *http.Response }, want int, what string) {
	t.Helper()
	if got := rec.Result().StatusCode; got != want {
		t.Fatalf("%s: status = %d, want %d", what, got, want)
	}
}

func TestHousesAccessControl(t *testing.T) {
	seed(t, "Admin H", "admin-h@example.test", "admin", nil)
	seed(t, "Guard H", "guard-h@example.test", "guard", nil)
	var houseID string
	if err := owner.QueryRow(t.Context(), `INSERT INTO houses (block, number) VALUES ('Z','1') RETURNING id`).Scan(&houseID); err != nil {
		t.Fatal(err)
	}
	seed(t, "Owner H", "owner-h@example.test", "owner", houseID)

	admin, guard, own := login(t, "admin-h@example.test"), login(t, "guard-h@example.test"), login(t, "owner-h@example.test")

	assertStatus(t, do(t, "GET", "/v1/houses", "", nil), http.StatusUnauthorized, "anonymous list")
	assertStatus(t, do(t, "GET", "/v1/houses", own.AccessToken, nil), http.StatusForbidden, "owner list")
	assertStatus(t, do(t, "GET", "/v1/houses", guard.AccessToken, nil), http.StatusOK, "guard list")
	assertStatus(t, do(t, "POST", "/v1/houses", guard.AccessToken, map[string]string{"number": "9"}), http.StatusForbidden, "guard create")
	assertStatus(t, do(t, "PATCH", "/v1/houses/"+houseID, guard.AccessToken, map[string]any{"active": false}), http.StatusForbidden, "guard update")
	assertStatus(t, do(t, "GET", "/v1/users", guard.AccessToken, nil), http.StatusForbidden, "guard list users")
	assertStatus(t, do(t, "GET", "/v1/users", own.AccessToken, nil), http.StatusForbidden, "owner list users")
	assertStatus(t, do(t, "GET", "/v1/users", admin.AccessToken, nil), http.StatusOK, "admin list users")
}

func TestHouseCRUD(t *testing.T) {
	seed(t, "Admin C", "admin-c@example.test", "admin", nil)
	seed(t, "Guard C", "guard-c@example.test", "guard", nil)
	admin, guard := login(t, "admin-c@example.test"), login(t, "guard-c@example.test")

	rec := do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": " b ", "number": " 12 "})
	assertStatus(t, rec, http.StatusCreated, "create")
	h := decode[houseBody](t, rec)
	if h.Block != "B" || h.Number != "12" || !h.Active {
		t.Fatalf("not normalized: %+v", h)
	}

	// Same block+number conflicts, regardless of block case.
	assertStatus(t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "B", "number": "12"}), http.StatusConflict, "duplicate")
	assertStatus(t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "b", "number": "12"}), http.StatusConflict, "duplicate other case")

	// Validation.
	assertStatus(t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "B"}), http.StatusUnprocessableEntity, "missing number")
	assertStatus(t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"number": strings.Repeat("9", 21)}), http.StatusUnprocessableEntity, "number too long")
	assertStatus(t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]any{"number": "1", "extra": true}), http.StatusBadRequest, "unknown field")

	// Update: rename, then conflict on rename into an existing house.
	rec = do(t, "PATCH", "/v1/houses/"+h.ID, admin.AccessToken, map[string]string{"number": "13"})
	assertStatus(t, rec, http.StatusOK, "rename")
	if got := decode[houseBody](t, rec); got.Number != "13" || got.Block != "B" {
		t.Fatalf("rename result: %+v", got)
	}
	other := decode[houseBody](t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "B", "number": "14"}))
	assertStatus(t, do(t, "PATCH", "/v1/houses/"+other.ID, admin.AccessToken, map[string]string{"number": "13"}), http.StatusConflict, "rename conflict")
	assertStatus(t, do(t, "PATCH", "/v1/houses/"+h.ID, admin.AccessToken, map[string]any{}), http.StatusUnprocessableEntity, "empty patch")

	// Deactivate: hidden from guards and from the default admin list, visible with include_inactive.
	assertStatus(t, do(t, "PATCH", "/v1/houses/"+h.ID, admin.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate")
	has := func(bearer, query string) bool {
		list := decode[struct{ Data []houseBody }](t, do(t, "GET", "/v1/houses"+query, bearer, nil))
		for _, x := range list.Data {
			if x.ID == h.ID {
				return true
			}
		}
		return false
	}
	if has(guard.AccessToken, "") || has(guard.AccessToken, "?include_inactive=true") {
		t.Fatal("guard must never see inactive houses")
	}
	if has(admin.AccessToken, "") {
		t.Fatal("inactive house in default admin list")
	}
	if !has(admin.AccessToken, "?include_inactive=true") {
		t.Fatal("admin include_inactive should show it")
	}

	assertStatus(t, do(t, "GET", "/v1/houses/"+h.ID, guard.AccessToken, nil), http.StatusOK, "get")
	assertStatus(t, do(t, "GET", "/v1/houses/00000000-0000-0000-0000-000000000000", guard.AccessToken, nil), http.StatusNotFound, "unknown id")
	assertStatus(t, do(t, "GET", "/v1/houses/not-a-uuid", guard.AccessToken, nil), http.StatusNotFound, "malformed id")

	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action IN ('house_updated')`, "house:"+h.ID); n != 2 {
		t.Fatalf("house_updated audit rows = %d, want 2", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'house_created'`, "house:"+h.ID); n != 1 {
		t.Fatalf("house_created audit rows = %d, want 1", n)
	}
}

func TestUserAccounts(t *testing.T) {
	seed(t, "Admin U", "admin-u@example.test", "admin", nil)
	admin := login(t, "admin-u@example.test")
	adminID := decode[struct {
		User struct{ ID string } `json:"user"`
	}](t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "admin-u@example.test", "password": testPassword})).User.ID

	hs := decode[houseBody](t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "U", "number": "1"}))
	inactive := decode[houseBody](t, do(t, "POST", "/v1/houses", admin.AccessToken, map[string]string{"block": "U", "number": "2"}))
	do(t, "PATCH", "/v1/houses/"+inactive.ID, admin.AccessToken, map[string]any{"active": false})

	newUser := func(body map[string]any) (int, userBody, string) {
		rec := do(t, "POST", "/v1/users", admin.AccessToken, body)
		return rec.Code, decode[userBody](t, rec), rec.Body.String()
	}
	owner1 := map[string]any{"name": "Budi Test", "email": " Budi.Owner@Example.Test ", "password": testPassword, "role": "owner", "house_id": hs.ID}

	code, u, raw := newUser(owner1)
	if code != http.StatusCreated {
		t.Fatalf("create owner = %d %s", code, raw)
	}
	if u.Email != "budi.owner@example.test" || u.Role != "owner" || u.HouseID == nil || *u.HouseID != hs.ID || !u.Active {
		t.Fatalf("unexpected owner: %+v", u)
	}
	if strings.Contains(raw, "password") {
		t.Fatalf("response leaks password data: %s", raw)
	}

	// The new owner can log in and is bound to the house in the token.
	ownerTok := login(t, "budi.owner@example.test")
	me := decode[struct {
		HouseID *string `json:"house_id"`
		Role    string  `json:"role"`
	}](t, do(t, "GET", "/v1/auth/me", ownerTok.AccessToken, nil))
	if me.Role != "owner" || me.HouseID == nil || *me.HouseID != hs.ID {
		t.Fatalf("/me = %+v", me)
	}

	// Rejections.
	if code, _, _ := newUser(owner1); code != http.StatusConflict {
		t.Fatalf("duplicate email = %d, want 409", code)
	}
	cases := []struct {
		name string
		body map[string]any
	}{
		{"owner without house", map[string]any{"name": "X", "email": "x1@example.test", "password": testPassword, "role": "owner"}},
		{"guard with house", map[string]any{"name": "X", "email": "x2@example.test", "password": testPassword, "role": "guard", "house_id": hs.ID}},
		{"owner of inactive house", map[string]any{"name": "X", "email": "x3@example.test", "password": testPassword, "role": "owner", "house_id": inactive.ID}},
		{"owner of unknown house", map[string]any{"name": "X", "email": "x4@example.test", "password": testPassword, "role": "owner", "house_id": "00000000-0000-0000-0000-000000000000"}},
		{"short password", map[string]any{"name": "X", "email": "x5@example.test", "password": "short", "role": "guard"}},
		{"bad role", map[string]any{"name": "X", "email": "x6@example.test", "password": testPassword, "role": "root"}},
		{"bad email", map[string]any{"name": "X", "email": "nope", "password": testPassword, "role": "guard"}},
		{"blank name", map[string]any{"name": "  ", "email": "x7@example.test", "password": testPassword, "role": "guard"}},
	}
	for _, c := range cases {
		if code, _, raw := newUser(c.body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (%s)", c.name, code, raw)
		}
	}

	// A guard and filtering/pagination.
	if code, _, raw := newUser(map[string]any{"name": "Siti Guard", "email": "siti.guard@example.test", "password": testPassword, "role": "guard"}); code != http.StatusCreated {
		t.Fatalf("create guard = %d %s", code, raw)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/v1/users?limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := decode[struct {
			Data       []userBody `json:"data"`
			NextCursor *string    `json:"next_cursor"`
		}](t, do(t, "GET", path, admin.AccessToken, nil))
		if len(res.Data) != 1 {
			t.Fatalf("page %d has %d rows", page, len(res.Data))
		}
		seen = append(seen, res.Data[0].ID)
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
	}
	if len(seen) < 3 {
		t.Fatalf("pagination returned only %d users", len(seen))
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Fatalf("pagination repeated %s", id)
		}
		uniq[id] = true
	}
	owners := decode[struct{ Data []userBody }](t, do(t, "GET", "/v1/users?role=owner&house_id="+hs.ID, admin.AccessToken, nil))
	if len(owners.Data) != 1 || owners.Data[0].ID != u.ID {
		t.Fatalf("filtered owners = %+v", owners.Data)
	}
	assertStatus(t, do(t, "GET", "/v1/users?role=root", admin.AccessToken, nil), http.StatusUnprocessableEntity, "bad role filter")
	assertStatus(t, do(t, "GET", "/v1/users?cursor=garbage", admin.AccessToken, nil), http.StatusUnprocessableEntity, "bad cursor")

	// Updates: house change only for owners and only to an active house.
	assertStatus(t, do(t, "PATCH", "/v1/users/"+u.ID, admin.AccessToken, map[string]any{"house_id": inactive.ID}), http.StatusUnprocessableEntity, "move to inactive house")
	rec := do(t, "PATCH", "/v1/users/"+u.ID, admin.AccessToken, map[string]any{"contact": "0812-0000-0000", "name": "Budi Baru"})
	assertStatus(t, rec, http.StatusOK, "update contact/name")
	assertStatus(t, do(t, "PATCH", "/v1/users/"+adminID, admin.AccessToken, map[string]any{"house_id": hs.ID}), http.StatusUnprocessableEntity, "admin with house")

	// Deactivating yourself is refused.
	assertStatus(t, do(t, "PATCH", "/v1/users/"+adminID, admin.AccessToken, map[string]any{"active": false}), http.StatusConflict, "self deactivate")

	// Deactivating the owner ends their sessions and blocks login.
	assertStatus(t, do(t, "PATCH", "/v1/users/"+u.ID, admin.AccessToken, map[string]any{"active": false}), http.StatusOK, "deactivate owner")
	assertStatus(t, do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": ownerTok.RefreshToken}), http.StatusUnauthorized, "refresh after deactivate")
	assertStatus(t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "budi.owner@example.test", "password": testPassword}), http.StatusUnauthorized, "login after deactivate")

	// Reactivate with a new password: old password stops working.
	assertStatus(t, do(t, "PATCH", "/v1/users/"+u.ID, admin.AccessToken, map[string]any{"active": true, "password": "Another Pass 99"}), http.StatusOK, "reactivate + reset")
	assertStatus(t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "budi.owner@example.test", "password": testPassword}), http.StatusUnauthorized, "old password")
	assertStatus(t, do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "budi.owner@example.test", "password": "Another Pass 99"}), http.StatusOK, "new password")

	assertStatus(t, do(t, "GET", "/v1/users/"+u.ID, admin.AccessToken, nil), http.StatusOK, "get user")
	assertStatus(t, do(t, "GET", "/v1/users/00000000-0000-0000-0000-000000000000", admin.AccessToken, nil), http.StatusNotFound, "unknown user")

	for action, want := range map[string]int{"user_created": 1, "user_deactivated": 1} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = $2`, "user:"+u.ID, action); n != want {
			t.Errorf("%s audit rows = %d, want %d", action, n, want)
		}
	}
}
