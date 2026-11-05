package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type syncedVisit struct {
	ID                       string  `json:"id"`
	Status                   string  `json:"status"`
	CheckinAt                string  `json:"checkin_at"`
	CheckoutAt               *string `json:"checkout_at"`
	CheckoutBy               *string `json:"checkout_by"`
	ClientCapturedAt         *string `json:"client_captured_at"`
	ClientCheckoutCapturedAt *string `json:"client_checkout_captured_at"`
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func sameInstant(t *testing.T, got *string, want time.Time, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got null, want %s", what, rfc(want))
	}
	g, err := time.Parse(time.RFC3339, *got)
	if err != nil {
		t.Fatalf("%s: %q is not RFC 3339: %v", what, *got, err)
	}
	if d := g.Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("%s: got %s, want %s", what, g.UTC().Format(time.RFC3339), rfc(want))
	}
}

// S5: a device that was offline replays its check-ins and check-outs later. The server's clock stays
// authoritative; the device's own time is stored beside it.
func TestOfflineSync(t *testing.T) {
	seed(t, "Guard S5", "guard-s5@example.test", "guard", nil)
	seed(t, "Admin S5", "admin-s5@example.test", "admin", nil)
	guard, admin := login(t, "guard-s5@example.test"), login(t, "admin-s5@example.test")
	houseID := newHouse(t, "S5", "1", true)
	seed(t, "Owner S5", "owner-s5@example.test", "owner", houseID)
	ownerTok := login(t, "owner-s5@example.test")

	checkInBody := func(key uuid.UUID, capturedAt time.Time) map[string]any {
		return map[string]any{
			"house_id": houseID, "guest_name": "Zq Offline", "category": "tamu",
			"client_request_id": key.String(), "client_captured_at": rfc(capturedAt),
		}
	}

	// The device recorded the check-in two hours ago. The server stores that beside its own time and
	// keeps its own time as checkin_at.
	key := uuid.New()
	captured := time.Now().Add(-2 * time.Hour)
	rec := do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(key, captured))
	assertStatus(t, rec, http.StatusCreated, "offline check-in")
	v := decode[syncedVisit](t, rec)
	sameInstant(t, v.ClientCapturedAt, captured, "client_captured_at")
	serverNow := dbNow(t)
	checkin, err := time.Parse(time.RFC3339, v.CheckinAt)
	if err != nil || serverNow.Sub(checkin) > 10*time.Second || checkin.After(serverNow.Add(time.Second)) {
		t.Fatalf("checkin_at = %s (%v), want the server's receive time near %s, not the device time", v.CheckinAt, err, rfc(serverNow))
	}
	if v.ClientCheckoutCapturedAt != nil {
		t.Fatalf("client_checkout_captured_at = %v before any check-out", *v.ClientCheckoutCapturedAt)
	}
	var stored time.Time
	if err := owner.QueryRow(context.Background(), `SELECT client_captured_at FROM visits WHERE id = $1`, v.ID).Scan(&stored); err != nil ||
		stored.Sub(captured) > time.Second || captured.Sub(stored) > time.Second {
		t.Fatalf("stored client_captured_at = %v (%v), want %v", stored, err, captured)
	}

	// An online check-in carries no device time.
	online := decode[syncedVisit](t, do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "Zq Online", "category": "tamu"}))
	if online.ClientCapturedAt != nil {
		t.Fatalf("online check-in has client_captured_at = %v", *online.ClientCapturedAt)
	}

	// Multipart carries it too.
	mp := doForm(t, "/v1/visits", guard.AccessToken, map[string]string{
		"house_id": houseID, "guest_name": "Zq Form", "category": "tamu", "client_captured_at": rfc(captured)}, nil)
	assertStatus(t, mp, http.StatusCreated, "multipart offline check-in")
	sameInstant(t, decode[syncedVisit](t, mp).ClientCapturedAt, captured, "multipart client_captured_at")

	// A replay returns the original visit, whatever device time or window it now carries.
	replay := do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(key, time.Now().Add(-80*time.Hour)))
	assertStatus(t, replay, http.StatusOK, "replay of a synced check-in")
	if got := decode[syncedVisit](t, replay); got.ID != v.ID {
		t.Fatalf("replay returned visit %s, want %s", got.ID, v.ID)
	} else {
		sameInstant(t, got.ClientCapturedAt, captured, "replay keeps the original device time")
	}
	if n := count(t, `SELECT count(*) FROM visits WHERE client_request_id = $1`, key); n != 1 {
		t.Fatalf("%d visits for one client_request_id, want 1", n)
	}

	// Plausibility window: at most 5 minutes ahead of the server and at most 72 hours back.
	for name, c := range map[string]struct {
		at   time.Time
		want int
	}{
		"4 minutes ahead (clock skew)": {time.Now().Add(4 * time.Minute), http.StatusCreated},
		"71 hours ago":                 {time.Now().Add(-71 * time.Hour), http.StatusCreated},
		"10 minutes ahead":             {time.Now().Add(10 * time.Minute), http.StatusUnprocessableEntity},
		"a year ahead":                 {time.Now().AddDate(1, 0, 0), http.StatusUnprocessableEntity},
		"73 hours ago":                 {time.Now().Add(-73 * time.Hour), http.StatusUnprocessableEntity},
	} {
		rec := do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(uuid.New(), c.at))
		assertStatus(t, rec, c.want, "check-in "+name)
		if c.want == http.StatusUnprocessableEntity && !strings.Contains(rec.Body.String(), "client_captured_at") {
			t.Fatalf("%s: error should name client_captured_at: %s", name, rec.Body)
		}
	}
	rejected := uuid.New()
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(rejected, time.Now().Add(time.Hour))), http.StatusUnprocessableEntity, "future check-in")
	if n := count(t, `SELECT count(*) FROM visits WHERE client_request_id = $1`, rejected); n != 0 {
		t.Fatalf("a rejected check-in was stored (%d rows)", n)
	}
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, map[string]string{
		"house_id": houseID, "guest_name": "X", "category": "tamu", "client_captured_at": "kemarin"}, nil),
		http.StatusUnprocessableEntity, "multipart with a bad time format")
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken, map[string]any{
		"house_id": houseID, "guest_name": "X", "category": "tamu", "client_captured_at": "kemarin"}),
		http.StatusBadRequest, "json with a bad time format")

	// Check-out by the check-in's key: the device never learned the server's visit id.
	exitAt := time.Now().Add(-30 * time.Minute)
	rec = do(t, "POST", "/v1/visits/by-request/"+key.String()+"/checkout", guard.AccessToken, map[string]any{"client_captured_at": rfc(exitAt)})
	assertStatus(t, rec, http.StatusOK, "check-out by request")
	out := decode[syncedVisit](t, rec)
	if out.ID != v.ID || out.Status != "exited" || out.CheckoutAt == nil {
		t.Fatalf("check-out by request = %+v, want visit %s exited", out, v.ID)
	}
	sameInstant(t, out.ClientCheckoutCapturedAt, exitAt, "client_checkout_captured_at")
	if co, err := time.Parse(time.RFC3339, *out.CheckoutAt); err != nil || dbNow(t).Sub(co) > 10*time.Second {
		t.Fatalf("checkout_at = %v (%v), want the server's receive time", *out.CheckoutAt, err)
	}
	sameInstant(t, out.ClientCapturedAt, captured, "check-in device time is untouched by the check-out")

	// A second check-out, by id or by key, keeps the first exit and its device time, and is not audited again.
	later := time.Now().Add(-5 * time.Minute)
	again := decode[syncedVisit](t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", guard.AccessToken, map[string]any{"client_captured_at": rfc(later)}))
	if *again.CheckoutAt != *out.CheckoutAt {
		t.Fatalf("a repeat check-out changed checkout_at from %s to %s", *out.CheckoutAt, *again.CheckoutAt)
	}
	sameInstant(t, again.ClientCheckoutCapturedAt, exitAt, "a repeat keeps the first device exit time")
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+key.String()+"/checkout", guard.AccessToken, nil), http.StatusOK, "repeat by key")
	// ...even with a device time that would be rejected for a first check-out.
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", guard.AccessToken, map[string]any{"client_captured_at": rfc(time.Now().Add(-100 * time.Hour))}),
		http.StatusOK, "repeat with a stale time")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'visit_checked_out'`, "visit:"+v.ID); n != 1 {
		t.Fatalf("visit_checked_out audit rows = %d, want 1", n)
	}

	// A first check-out still validates the device time, and an empty body still works.
	k2 := uuid.New()
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(k2, time.Now())), http.StatusCreated, "second check-in")
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", guard.AccessToken, map[string]any{"client_captured_at": rfc(time.Now().Add(time.Hour))}),
		http.StatusUnprocessableEntity, "check-out with a future device time")
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", guard.AccessToken, map[string]any{"client_captured_at": rfc(time.Now().Add(-200 * time.Hour))}),
		http.StatusUnprocessableEntity, "check-out with a stale device time")
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", guard.AccessToken, map[string]any{"unknown_field": 1}),
		http.StatusBadRequest, "check-out with an unknown field")
	if st := decode[syncedVisit](t, do(t, "GET", "/v1/visits/"+decode[syncedVisit](t, do(t, "POST", "/v1/visits", guard.AccessToken, checkInBody(k2, time.Now()))).ID, guard.AccessToken, nil)).Status; st != "inside" {
		t.Fatalf("a refused check-out changed the visit, status = %q", st)
	}
	plain := do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", guard.AccessToken, nil)
	assertStatus(t, plain, http.StatusOK, "check-out with no body")
	if got := decode[syncedVisit](t, plain); got.ClientCheckoutCapturedAt != nil {
		t.Fatalf("an online check-out carries a device time: %v", *got.ClientCheckoutCapturedAt)
	}

	// Unknown or malformed keys, and who may use the route.
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+uuid.NewString()+"/checkout", guard.AccessToken, nil), http.StatusNotFound, "unknown key")
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/not-a-uuid/checkout", guard.AccessToken, nil), http.StatusNotFound, "malformed key")
	for name, tok := range map[string]string{"owner": ownerTok.AccessToken, "admin": admin.AccessToken} {
		assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", tok, nil), http.StatusForbidden, name+" check-out by key")
	}
	assertStatus(t, do(t, "POST", "/v1/visits/by-request/"+k2.String()+"/checkout", "", nil), http.StatusUnauthorized, "anonymous check-out by key")
}

// The server clock stays authoritative for everything that depends on time: a check-in recorded
// offline days ago still sorts, filters and ages by the time the server received it.
func TestOfflineTimeDoesNotMoveServerTime(t *testing.T) {
	seed(t, "Guard S5b", "guard-s5b@example.test", "guard", nil)
	guard := login(t, "guard-s5b@example.test")
	houseID := newHouse(t, "S5", "2", true)
	rec := do(t, "POST", "/v1/visits", guard.AccessToken, map[string]any{
		"house_id": houseID, "guest_name": "Zq Lama", "category": "tamu",
		"client_captured_at": rfc(time.Now().Add(-60 * time.Hour))})
	assertStatus(t, rec, http.StatusCreated, "check-in recorded 60 hours ago")
	v := decode[syncedVisit](t, rec)

	// Filtering by today's date finds it (checkin_at is today), and it is not flagged overstay.
	today := day(0)
	list := decode[visitList](t, do(t, "GET", "/v1/visits?house_id="+houseID+"&from="+today+"&to="+today, guard.AccessToken, nil))
	if len(list.Data) != 1 || list.Data[0].ID != v.ID {
		t.Fatalf("filtering by today's date returned %v, want the offline check-in", ids(list))
	}
	if got := decode[overstayVisit](t, do(t, "GET", "/v1/visits/"+v.ID, guard.AccessToken, nil)); got.Overstay {
		t.Fatal("overstay was computed from the device time instead of the server's check-in time")
	}
}
