package e2e

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// asRole runs fn in a transaction on the application database role, with the row-level-security
// context a request would carry (app.role and app.house_id), and rolls it back afterwards. It lets a
// test probe what the database itself allows, whatever the service does.
func asRole(t *testing.T, role string, house any, fn func(tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	h := ""
	if house != nil {
		h = house.(string)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.role', $1, true), set_config('app.house_id', $2, true)`, role, h); err != nil {
		t.Fatal(err)
	}
	fn(tx)
}

type decidedVisit struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	OwnerDecision struct {
		Status    string  `json:"status"`
		DecidedAt *string `json:"decided_at"`
		Source    *string `json:"source"`
	} `json:"owner_decision"`
}

func decisionBody(d string) map[string]any { return map[string]any{"decision": d} }

// S4: the owner confirms or rejects a visit and the guard sees the answer.
func TestOwnerDecision(t *testing.T) {
	f := newOwnerFixture(t, "OD")
	a1, a2 := f.visitsA[0].ID, f.visitsA[1].ID
	get := func(token, id string) decidedVisit {
		t.Helper()
		rec := do(t, "GET", "/v1/visits/"+id, token, nil)
		assertStatus(t, rec, http.StatusOK, "get visit")
		return decode[decidedVisit](t, rec)
	}
	list := func(token, query string) []string {
		t.Helper()
		rec := do(t, "GET", "/v1/visits?"+query, token, nil)
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

	// Nothing answered yet: pending for everyone, and the pending filter finds both visits.
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "owner": f.ownerA.AccessToken} {
		v := get(tok, a1)
		if v.OwnerDecision.Status != "pending" || v.OwnerDecision.DecidedAt != nil || v.OwnerDecision.Source != nil {
			t.Fatalf("%s sees %+v, want pending without a time or source", name, v.OwnerDecision)
		}
	}
	expect("owner pending", list(f.ownerA.AccessToken, "status=inside&decision=pending"), want(a1, a2))

	// The owner rejects one visit; the guard sees it on the visit and in the inside list.
	rec := do(t, "POST", "/v1/visits/"+a1+"/decision", f.ownerA.AccessToken, decisionBody("rejected"))
	assertStatus(t, rec, http.StatusOK, "owner rejects")
	got := decode[decidedVisit](t, rec)
	if got.OwnerDecision.Status != "rejected" || got.OwnerDecision.DecidedAt == nil ||
		got.OwnerDecision.Source == nil || *got.OwnerDecision.Source != "owner" {
		t.Fatalf("reply = %+v, want rejected by the owner with a time", got.OwnerDecision)
	}
	if v := get(f.guard.AccessToken, a1); v.OwnerDecision.Status != "rejected" {
		t.Fatalf("guard sees %+v after the owner rejected", v.OwnerDecision)
	}
	expect("guard rejected in house A", list(f.guard.AccessToken, "status=inside&decision=rejected&house_id="+f.houseA), want(a1))
	expect("guard pending in house A", list(f.guard.AccessToken, "status=inside&decision=pending&house_id="+f.houseA), want(a2))

	// The same answer again is harmless and not audited twice; a different one is refused.
	assertStatus(t, do(t, "POST", "/v1/visits/"+a1+"/decision", f.ownerA.AccessToken, decisionBody("rejected")), http.StatusOK, "same answer again")
	rec = do(t, "POST", "/v1/visits/"+a1+"/decision", f.ownerA.AccessToken, decisionBody("approved"))
	assertStatus(t, rec, http.StatusConflict, "changing the answer")
	if v := get(f.guard.AccessToken, a1); v.OwnerDecision.Status != "rejected" {
		t.Fatalf("the answer changed to %q", v.OwnerDecision.Status)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target = $1 AND action = 'visit_decided'`, "visit:"+a1); n != 1 {
		t.Fatalf("visit_decided audit rows = %d, want 1", n)
	}

	assertStatus(t, do(t, "POST", "/v1/visits/"+a2+"/decision", f.ownerA.AccessToken, decisionBody("approved")), http.StatusOK, "owner approves")
	expect("approved", list(f.ownerA.AccessToken, "decision=approved"), want(a2))
	expect("pending is now empty", list(f.ownerA.AccessToken, "status=inside&decision=pending"), want())

	// The answer is advice: a rejected guest can still be checked out.
	assertStatus(t, do(t, "POST", "/v1/visits/"+a1+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout after rejection")
	// ...and repeating the same answer after check-out still works, while a new answer does not.
	assertStatus(t, do(t, "POST", "/v1/visits/"+a1+"/decision", f.ownerA.AccessToken, decisionBody("rejected")), http.StatusOK, "same answer after checkout")

	// Another house's visit looks exactly like a missing one, and nothing is written.
	missing := do(t, "POST", "/v1/visits/"+uuid.NewString()+"/decision", f.ownerB.AccessToken, decisionBody("approved"))
	cross := do(t, "POST", "/v1/visits/"+a2+"/decision", f.ownerB.AccessToken, decisionBody("rejected"))
	assertStatus(t, cross, http.StatusNotFound, "owner B decides on A's visit")
	if cross.Body.String() != missing.Body.String() {
		t.Fatalf("cross-house 404 differs from a missing id:\n%s\n%s", cross.Body, missing.Body)
	}
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitB.ID+"/decision", f.ownerA.AccessToken, decisionBody("approved")), http.StatusNotFound, "owner A decides on B's visit")
	if n := count(t, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, f.visitB.ID); n != 0 {
		t.Fatalf("a cross-house decision was stored (%d rows)", n)
	}

	// Only the house's owner answers.
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitB.ID+"/decision", f.guard.AccessToken, decisionBody("approved")), http.StatusForbidden, "guard decides")
	seed(t, "Admin OD", "admin-od@example.test", "admin", nil)
	admin := login(t, "admin-od@example.test")
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitB.ID+"/decision", admin.AccessToken, decisionBody("approved")), http.StatusForbidden, "admin decides")
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitB.ID+"/decision", "", decisionBody("approved")), http.StatusUnauthorized, "anonymous decides")

	// Bad input.
	for name, body := range map[string]map[string]any{
		"unknown answer": decisionBody("maybe"), "empty answer": decisionBody(""), "no body fields": {},
	} {
		assertStatus(t, do(t, "POST", "/v1/visits/"+f.visitB.ID+"/decision", f.ownerB.AccessToken, body), http.StatusUnprocessableEntity, name)
	}
	assertStatus(t, do(t, "POST", "/v1/visits/not-a-uuid/decision", f.ownerB.AccessToken, decisionBody("approved")), http.StatusNotFound, "bad id")
	assertStatus(t, do(t, "GET", "/v1/visits?decision=maybe", f.guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad filter")

	// A guest who already left cannot be answered about.
	late := doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{"house_id": f.houseA, "guest_name": "Zq Sudah Pergi", "category": "tamu"}, nil)
	assertStatus(t, late, http.StatusCreated, "check-in")
	lateID := decode[visitBody](t, late).ID
	assertStatus(t, do(t, "POST", "/v1/visits/"+lateID+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout")
	assertStatus(t, do(t, "POST", "/v1/visits/"+lateID+"/decision", f.ownerA.AccessToken, decisionBody("approved")), http.StatusConflict, "answer after checkout")
	if n := count(t, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, lateID); n != 0 {
		t.Fatalf("a decision was stored for a visit that had left (%d rows)", n)
	}

	// Audit targets are visit ids only.
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visit_decided' AND target NOT LIKE 'visit:%'`); n != 0 {
		t.Fatalf("%d visit_decided audit rows have an unexpected target", n)
	}
}

// A visit made from a pre-registration counts as approved without any row being written; the owner can
// still reject it once.
func TestOwnerDecisionForPreRegisteredVisit(t *testing.T) {
	f := newPreFixture(t, "OP")
	pre := mustCreatePre(t, f.ownerA.AccessToken, "Zqtamu Setuju", day(0), "")

	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": pre.Code})
	assertStatus(t, rec, http.StatusCreated, "check-in with code")
	v := decode[decidedVisit](t, rec)
	if v.OwnerDecision.Status != "approved" || v.OwnerDecision.Source == nil || *v.OwnerDecision.Source != "pre_registration" ||
		v.OwnerDecision.DecidedAt != nil {
		t.Fatalf("pre-registered visit = %+v, want approved by the pre-registration, no decision time", v.OwnerDecision)
	}
	if n := count(t, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, v.ID); n != 0 {
		t.Fatalf("a decision row was written for a pre-registered visit (%d)", n)
	}
	approved := decode[visitList](t, do(t, "GET", "/v1/visits?decision=approved&house_id="+f.houseA, f.guard.AccessToken, nil))
	if !slices.Contains(ids(approved), v.ID) {
		t.Fatalf("approved filter misses the pre-registered visit: %v", ids(approved))
	}
	pending := decode[visitList](t, do(t, "GET", "/v1/visits?decision=pending&house_id="+f.houseA, f.guard.AccessToken, nil))
	if slices.Contains(ids(pending), v.ID) {
		t.Fatal("pending filter returns a pre-registered visit")
	}

	// The owner may still change their mind, once.
	rec = do(t, "POST", "/v1/visits/"+v.ID+"/decision", f.ownerA.AccessToken, decisionBody("rejected"))
	assertStatus(t, rec, http.StatusOK, "owner rejects a pre-registered visit")
	if got := decode[decidedVisit](t, rec); got.OwnerDecision.Status != "rejected" || got.OwnerDecision.Source == nil || *got.OwnerDecision.Source != "owner" {
		t.Fatalf("after rejecting: %+v", got.OwnerDecision)
	}
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/decision", f.ownerA.AccessToken, decisionBody("approved")), http.StatusConflict, "second change")
}

// The database enforces the same rules whatever the service does: row-level security, the composite
// foreign key, and the app role having no UPDATE or DELETE on decisions.
func TestVisitDecisionsAreAppendOnlyAndIsolated(t *testing.T) {
	f := newOwnerFixture(t, "OR")
	a1 := f.visitsA[0].ID
	assertStatus(t, do(t, "POST", "/v1/visits/"+a1+"/decision", f.ownerA.AccessToken, decisionBody("approved")), http.StatusOK, "owner A approves")
	ctx := context.Background()
	ownerAUser, ownerBUser := f.ownerA.User.ID, f.ownerB.User.ID

	asRole(t, "owner", f.houseB, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, a1).Scan(&n); err != nil || n != 0 {
			t.Fatalf("owner B reads A's decision through RLS: n=%d err=%v", n, err)
		}
	})
	asRole(t, "owner", f.houseA, func(tx pgx.Tx) {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, a1).Scan(&n); err != nil || n != 1 {
			t.Fatalf("owner A cannot read their own decision: n=%d err=%v", n, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE visit_decisions SET decision = 'rejected' WHERE visit_id = $1`, a1); err == nil {
			t.Fatal("the app role can update a decision")
		}
	})
	asRole(t, "owner", f.houseA, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `DELETE FROM visit_decisions WHERE visit_id = $1`, a1); err == nil {
			t.Fatal("the app role can delete a decision")
		}
	})
	// An owner cannot write a decision for another house, or for a visit that is not their house's.
	asRole(t, "owner", f.houseB, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO visit_decisions (visit_id, house_id, decision, decided_by) VALUES ($1, $2, 'rejected', $3)`,
			f.visitsA[1].ID, f.houseA, ownerBUser); err == nil {
			t.Fatal("owner B stored a decision for house A through RLS")
		}
	})
	asRole(t, "owner", f.houseB, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `INSERT INTO visit_decisions (visit_id, house_id, decision, decided_by) VALUES ($1, $2, 'rejected', $3)`,
			f.visitsA[1].ID, f.houseB, ownerBUser); err == nil {
			t.Fatal("a decision was stored with a house that does not own the visit (composite foreign key)")
		}
	})
	// Guards and admins read decisions but cannot write them.
	for _, role := range []string{"guard", "admin"} {
		asRole(t, role, nil, func(tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM visit_decisions WHERE visit_id = $1`, a1).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s cannot read the decision: n=%d err=%v", role, n, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO visit_decisions (visit_id, house_id, decision, decided_by) VALUES ($1, $2, 'approved', $3)`,
				f.visitsA[1].ID, f.houseA, ownerAUser); err == nil {
				t.Fatalf("%s stored a decision through RLS", role)
			}
		})
	}
}
