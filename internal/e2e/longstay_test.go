package e2e

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

type overstayVisit struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Overstay bool   `json:"overstay"`
}

// setLongStay writes the raw setting value as the table owner (so tests can store corrupt values too)
// and restores the default when the test ends.
func setLongStay(t *testing.T, value string) {
	t.Helper()
	ctx := context.Background()
	if _, err := owner.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('long_stay_hours', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `DELETE FROM settings WHERE key = 'long_stay_hours'`) })
}

// ageVisit moves a visit's check-in back by hours (the owner pool bypasses RLS).
func ageVisit(t *testing.T, id string, hours int) {
	t.Helper()
	if _, err := owner.Exec(context.Background(),
		`UPDATE visits SET checkin_at = now() - make_interval(hours => $2) WHERE id = $1`, id, hours); err != nil {
		t.Fatal(err)
	}
}

// S7: a guest still inside after the configured number of hours is flagged.
func TestLongStayFlag(t *testing.T) {
	seed(t, "Guard S7", "guard-s7@example.test", "guard", nil)
	seed(t, "Admin S7", "admin-s7@example.test", "admin", nil)
	guard, admin := login(t, "guard-s7@example.test"), login(t, "admin-s7@example.test")
	houseID := newHouse(t, "S7", "1", true)
	seed(t, "Owner S7", "owner-s7@example.test", "owner", houseID)
	ownerTok := login(t, "owner-s7@example.test")

	checkIn := func(name string) string {
		rec := do(t, "POST", "/v1/visits", guard.AccessToken, map[string]any{"house_id": houseID, "guest_name": name, "category": "tamu"})
		assertStatus(t, rec, http.StatusCreated, "check-in "+name)
		return decode[visitBody](t, rec).ID
	}
	fresh, eleven, thirteen, left := checkIn("Zq Baru"), checkIn("Zq Sebelas"), checkIn("Zq Tigabelas"), checkIn("Zq Pergi")
	ageVisit(t, eleven, 11)
	ageVisit(t, thirteen, 13)
	ageVisit(t, left, 30)
	assertStatus(t, do(t, "POST", "/v1/visits/"+left+"/checkout", guard.AccessToken, nil), http.StatusOK, "checkout")

	get := func(token, id string) overstayVisit {
		t.Helper()
		rec := do(t, "GET", "/v1/visits/"+id, token, nil)
		assertStatus(t, rec, http.StatusOK, "get visit")
		return decode[overstayVisit](t, rec)
	}
	flagged := func(token, query string) []string {
		t.Helper()
		rec := do(t, "GET", "/v1/visits?house_id="+houseID+"&"+query, token, nil)
		assertStatus(t, rec, http.StatusOK, "list "+query)
		got := ids(decode[visitList](t, rec))
		slices.Sort(got)
		return got
	}
	want := func(vs ...string) []string { slices.Sort(vs); return vs }
	expect := func(what string, got, wantIDs []string) {
		t.Helper()
		if !slices.Equal(got, wantIDs) {
			t.Fatalf("%s: got %v, want %v", what, got, wantIDs)
		}
	}

	// Default limit is 12 hours: 13 h is flagged, 11 h and a fresh guest are not, and neither is a
	// guest who already left, however long they stayed.
	for name, c := range map[string]struct {
		id   string
		want bool
	}{"fresh": {fresh, false}, "11 hours": {eleven, false}, "13 hours": {thirteen, true}, "left after 30 hours": {left, false}} {
		if v := get(guard.AccessToken, c.id); v.Overstay != c.want {
			t.Fatalf("%s: overstay = %v, want %v", name, v.Overstay, c.want)
		}
	}
	// The owner sees the flag for their own house too.
	if v := get(ownerTok.AccessToken, thirteen); !v.Overstay {
		t.Fatal("owner does not see the overstay flag on their own visit")
	}

	// Filter, on the inside list and on the general list, for guards and owners.
	expect("overstay on the general list", flagged(guard.AccessToken, "overstay=true"), want(thirteen))
	expect("overstay on the inside list", flagged(guard.AccessToken, "status=inside&overstay=true"), want(thirteen))
	expect("overstay as owner", flagged(ownerTok.AccessToken, "overstay=true"), want(thirteen))
	expect("overstay=false is no filter", flagged(guard.AccessToken, "overstay=false&status=inside"), want(fresh, eleven, thirteen))
	expect("overstay combined with exited", flagged(guard.AccessToken, "status=exited&overstay=true"), want())
	assertStatus(t, do(t, "GET", "/v1/visits?overstay=maybe", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad filter value")

	// The limit is the admin's to change, and takes effect straight away.
	rec := do(t, "GET", "/v1/settings/long-stay", admin.AccessToken, nil)
	assertStatus(t, rec, http.StatusOK, "get setting")
	if got := decode[struct {
		Hours int `json:"long_stay_hours"`
	}](t, rec).Hours; got != 12 {
		t.Fatalf("default limit = %d, want 12", got)
	}
	assertStatus(t, do(t, "PUT", "/v1/settings/long-stay", admin.AccessToken, map[string]any{"long_stay_hours": 6}), http.StatusOK, "set 6 hours")
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM settings WHERE key = 'long_stay_hours'`) })
	expect("limit 6", flagged(guard.AccessToken, "status=inside&overstay=true"), want(eleven, thirteen))
	assertStatus(t, do(t, "PUT", "/v1/settings/long-stay", admin.AccessToken, map[string]any{"long_stay_hours": 24}), http.StatusOK, "set 24 hours")
	expect("limit 24", flagged(guard.AccessToken, "status=inside&overstay=true"), want())
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'long_stay_settings_changed' AND target = 'long_stay_hours:12->6'`); n == 0 {
		t.Fatal("changing the limit was not audited with the old and new value")
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'long_stay_settings_changed' AND target = 'long_stay_hours:6->24'`); n == 0 {
		t.Fatal("the second change was not audited")
	}

	// Validation and access.
	for name, body := range map[string]map[string]any{
		"zero": {"long_stay_hours": 0}, "negative": {"long_stay_hours": -3}, "over a week": {"long_stay_hours": 169}, "missing": {},
	} {
		assertStatus(t, do(t, "PUT", "/v1/settings/long-stay", admin.AccessToken, body), http.StatusUnprocessableEntity, name)
	}
	for name, tok := range map[string]string{"guard": guard.AccessToken, "owner": ownerTok.AccessToken} {
		assertStatus(t, do(t, "GET", "/v1/settings/long-stay", tok, nil), http.StatusForbidden, name+" read")
		assertStatus(t, do(t, "PUT", "/v1/settings/long-stay", tok, map[string]any{"long_stay_hours": 6}), http.StatusForbidden, name+" write")
	}
	assertStatus(t, do(t, "GET", "/v1/settings/long-stay", "", nil), http.StatusUnauthorized, "anonymous read")

	// A corrupt stored value never breaks the visit lists: the SQL falls back to 12 hours. The admin
	// endpoint reports it instead of hiding it, and can still overwrite it.
	for _, bad := range []string{"abc", "-5", "0", "99999", "1.5", ""} {
		setLongStay(t, bad)
		expect("corrupt value "+bad, flagged(guard.AccessToken, "status=inside&overstay=true"), want(thirteen))
	}
	setLongStay(t, "abc")
	assertStatus(t, do(t, "GET", "/v1/settings/long-stay", admin.AccessToken, nil), http.StatusInternalServerError, "corrupt value is reported")
	assertStatus(t, do(t, "PUT", "/v1/settings/long-stay", admin.AccessToken, map[string]any{"long_stay_hours": 12}), http.StatusOK, "fix a corrupt value")
}
