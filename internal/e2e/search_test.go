package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// S6: guard search and filter by name, plate and category, on top of house/date/status.
func TestVisitSearch(t *testing.T) {
	seed(t, "Guard S6", "guard-s6@example.test", "guard", nil)
	guard := login(t, "guard-s6@example.test")
	houseA := newHouse(t, "S6", "A", true)
	houseB := newHouse(t, "S6", "B", true)
	seed(t, "Owner S6", "owner-s6@example.test", "owner", houseA)
	ownerA := login(t, "owner-s6@example.test")

	// Names and plates carry a unique token (zq) so other tests' rows never match.
	photo := map[string][]byte{"vehicle_photo": testJPEG(t, 60, 40, "")}
	checkIn := func(fields map[string]string, files map[string][]byte) visitBody {
		t.Helper()
		rec := doForm(t, "/v1/visits", guard.AccessToken, fields, files)
		assertStatus(t, rec, http.StatusCreated, "fixture check-in")
		return decode[visitBody](t, rec)
	}
	budiA := checkIn(map[string]string{"house_id": houseA, "guest_name": "Zqbudi Santoso", "category": "tamu", "vehicle_plate": "B 4321 ZQA"}, photo)
	sitiA := checkIn(map[string]string{"house_id": houseA, "guest_name": "Zqsiti Rahma", "category": "tukang"}, nil)
	kurirA := checkIn(map[string]string{"house_id": houseA, "category": "kurir", "vehicle_plate": "B 7777 ZQK"}, nil)
	budiB := checkIn(map[string]string{"house_id": houseB, "guest_name": "Zqbudi Gunawan", "category": "tamu"}, nil)

	search := func(token string, q url.Values) []string {
		t.Helper()
		rec := do(t, "GET", "/v1/visits?"+q.Encode(), token, nil)
		assertStatus(t, rec, http.StatusOK, "search "+q.Encode())
		got := ids(decode[visitList](t, rec))
		slices.Sort(got)
		return got
	}
	want := func(vs ...visitBody) []string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = v.ID
		}
		slices.Sort(out)
		return out
	}
	expect := func(what string, got, wantIDs []string) {
		t.Helper()
		if !slices.Equal(got, wantIDs) {
			t.Fatalf("%s: got %v, want %v", what, got, wantIDs)
		}
	}

	// Name: case-insensitive contains, across houses for a guard.
	expect("name zqbudi", search(guard.AccessToken, url.Values{"name": {"ZQBUDI"}}), want(budiA, budiB))
	expect("name partial", search(guard.AccessToken, url.Values{"name": {"santoso"}}), want(budiA))
	expect("name + house", search(guard.AccessToken, url.Values{"name": {"zqbudi"}, "house_id": {houseB}}), want(budiB))

	// Plate: contains, ignoring case and spaces on both sides.
	expect("plate compact", search(guard.AccessToken, url.Values{"plate": {"b4321zqa"}}), want(budiA))
	expect("plate spaced", search(guard.AccessToken, url.Values{"plate": {"4321 zq"}}), want(budiA))
	expect("plate other", search(guard.AccessToken, url.Values{"plate": {"7777"}}), want(kurirA))

	// Category, alone and combined with the other filters.
	expect("category kurir in house", search(guard.AccessToken, url.Values{"category": {"kurir"}, "house_id": {houseA}}), want(kurirA))
	expect("category + name", search(guard.AccessToken, url.Values{"category": {"tukang"}, "name": {"zq"}}), want(sitiA))

	// Status: the inside list (partial-index query) and the general list both honour the search.
	assertStatus(t, do(t, "POST", "/v1/visits/"+sitiA.ID+"/checkout", guard.AccessToken, nil), http.StatusOK, "checkout")
	expect("inside + name", search(guard.AccessToken, url.Values{"status": {"inside"}, "name": {"zqsiti"}}), want())
	expect("exited + name", search(guard.AccessToken, url.Values{"status": {"exited"}, "name": {"zqsiti"}}), want(sitiA))
	expect("inside + plate", search(guard.AccessToken, url.Values{"status": {"inside"}, "plate": {"4321"}}), want(budiA))

	// User text is matched literally: LIKE metacharacters never act as wildcards.
	expect("percent is literal", search(guard.AccessToken, url.Values{"name": {"%%"}}), want())
	expect("underscore is literal", search(guard.AccessToken, url.Values{"name": {"zq_udi"}}), want())
	expect("backslash is literal", search(guard.AccessToken, url.Values{"name": {`zq\`}}), want())

	// Invalid input is refused with field details.
	for name, q := range map[string]url.Values{
		"name too short":    {"name": {"a"}},
		"plate too short":   {"plate": {"b"}},
		"plate with symbol": {"plate": {"B-1234"}},
		"unknown category":  {"category": {"vip"}},
	} {
		assertStatus(t, do(t, "GET", "/v1/visits?"+q.Encode(), guard.AccessToken, nil), http.StatusUnprocessableEntity, name)
	}

	// Pagination keeps the filter: two matches, one per page, no repeats.
	page1 := decode[visitList](t, do(t, "GET", "/v1/visits?"+url.Values{"name": {"zqbudi"}, "limit": {"1"}}.Encode(), guard.AccessToken, nil))
	if len(page1.Data) != 1 || page1.NextCursor == nil {
		t.Fatalf("page 1 = %v next=%v, want 1 row and a cursor", ids(page1), page1.NextCursor)
	}
	page2 := decode[visitList](t, do(t, "GET", "/v1/visits?"+url.Values{"name": {"zqbudi"}, "limit": {"1"}, "cursor": {*page1.NextCursor}}.Encode(), guard.AccessToken, nil))
	if len(page2.Data) != 1 || page2.Data[0].ID == page1.Data[0].ID || page2.NextCursor != nil {
		t.Fatalf("page 2 = %v next=%v, want the other row and no cursor", ids(page2), page2.NextCursor)
	}

	// An owner can search, but only inside their own house.
	expect("owner name", search(ownerA.AccessToken, url.Values{"name": {"zqbudi"}}), want(budiA))
	expect("owner other house", search(ownerA.AccessToken, url.Values{"name": {"zqbudi"}, "house_id": {houseB}}), want())

	// Guard searches are not audited (like the guard list); an owner's are, and the audit target
	// never carries the search text.
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE user_id = $1 AND action = 'visits_listed'`, guard.User.ID); n != 0 {
		t.Fatalf("guard list/search audit rows = %d, want 0", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE user_id = $1 AND action = 'visits_listed'`, ownerA.User.ID); n == 0 {
		t.Fatal("owner search was not audited")
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target ILIKE '%zqbudi%' OR target ILIKE '%4321%'`); n != 0 {
		t.Fatalf("audit targets contain search text (%d rows)", n)
	}
}
