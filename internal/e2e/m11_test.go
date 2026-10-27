package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type auditEntry struct {
	ID        int64   `json:"id"`
	UserID    *string `json:"user_id"`
	UserName  *string `json:"user_name"`
	Action    string  `json:"action"`
	Target    string  `json:"target"`
	CreatedAt string  `json:"created_at"`
}

type auditList struct {
	Data       []auditEntry `json:"data"`
	NextCursor *string      `json:"next_cursor"`
}

func fetchAudit(t *testing.T, bearer string, q url.Values) auditList {
	t.Helper()
	rec := do(t, "GET", "/v1/audit-logs?"+q.Encode(), bearer, nil)
	assertStatus(t, rec, http.StatusOK, "audit list "+q.Encode())
	return decode[auditList](t, rec)
}

func TestAuditLogAccessAndImmutability(t *testing.T) {
	seed(t, "Admin AL", "admin-al@example.test", "admin", nil)
	seed(t, "Guard AL", "guard-al@example.test", "guard", nil)
	houseID := newHouse(t, "AL", "1", true)
	seed(t, "Owner AL", "owner-al@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-al@example.test"), login(t, "guard-al@example.test"), login(t, "owner-al@example.test")

	// Only admins can read it.
	assertStatus(t, do(t, "GET", "/v1/audit-logs", "", nil), http.StatusUnauthorized, "anonymous")
	assertStatus(t, do(t, "GET", "/v1/audit-logs", guard.AccessToken, nil), http.StatusForbidden, "guard")
	assertStatus(t, do(t, "GET", "/v1/audit-logs", own.AccessToken, nil), http.StatusForbidden, "owner")
	assertStatus(t, do(t, "GET", "/v1/audit-logs", admin.AccessToken, nil), http.StatusOK, "admin")

	// The API has no way to write, change or delete entries, not even for admins.
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{"/v1/audit-logs", "/v1/audit-logs/1"} {
			if code := do(t, m, path, admin.AccessToken, map[string]string{"action": "x"}).Code; code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404 or 405", m, path, code)
			}
		}
	}

	// And the database role the API runs as cannot edit or delete rows either, so a
	// bug or a stolen credential in the app still cannot rewrite history.
	ctx := context.Background()
	if _, err := app.Exec(ctx, `INSERT INTO audit_logs (action, target) VALUES ('test_insert', 'test')`); err != nil {
		t.Fatalf("app role must be able to append: %v", err)
	}
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("app role must be able to read: n=%d err=%v", n, err)
	}
	for name, stmt := range map[string]string{
		"update":   `UPDATE audit_logs SET action = 'tampered'`,
		"delete":   `DELETE FROM audit_logs`,
		"truncate": `TRUNCATE audit_logs`,
	} {
		_, err := app.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" { // insufficient_privilege
			t.Errorf("%s by the app role: err = %v, want permission denied (42501)", name, err)
		}
	}
	if got := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'tampered'`); got != 0 {
		t.Fatalf("%d rows were tampered with", got)
	}
}

func TestAuditLogRecordsWhoSawAndChangedWhat(t *testing.T) {
	f := newOwnerFixture(t, "AU")
	seed(t, "Admin AU", "admin-au@example.test", "admin", nil)
	admin := login(t, "admin-au@example.test")
	v := f.visitsA[0]

	// Things people do. Each should leave a trace naming who did it.
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID, f.ownerA.AccessToken, nil), http.StatusOK, "owner views a visit")
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID+"/photos/vehicle", f.ownerA.AccessToken, nil), http.StatusOK, "owner views a photo")
	assertStatus(t, do(t, "GET", "/v1/visits", f.ownerA.AccessToken, nil), http.StatusOK, "owner lists history")
	assertStatus(t, do(t, "GET", "/v1/visits?status=inside", f.guard.AccessToken, nil), http.StatusOK, "guard polls the inside list")
	assertStatus(t, do(t, "GET", "/v1/visits?house_id="+f.houseA, admin.AccessToken, nil), http.StatusOK, "admin lists one house")
	assertStatus(t, do(t, "GET", "/v1/visits", admin.AccessToken, nil), http.StatusOK, "admin lists everything")
	assertStatus(t, do(t, "GET", "/v1/visits/"+f.visitB.ID, f.ownerA.AccessToken, nil), http.StatusNotFound, "owner A probes house B")
	assertStatus(t, do(t, "POST", "/v1/visits/"+v.ID+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "guard checks out")
	do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "nobody-au@example.test", "password": "whatever it is"})

	// Everything about one visit, newest first.
	byVisit := fetchAudit(t, admin.AccessToken, url.Values{"target": {"visit:" + v.ID}, "limit": {"200"}})
	type key struct{ action, user string }
	got := map[key]bool{}
	for i, e := range byVisit.Data {
		if e.UserID == nil || e.UserName == nil {
			t.Fatalf("entry %d has no actor: %+v", e.ID, e)
		}
		got[key{e.Action, *e.UserID}] = true
		if i > 0 && e.ID >= byVisit.Data[i-1].ID {
			t.Fatalf("not newest first: %d after %d", e.ID, byVisit.Data[i-1].ID)
		}
		if len(e.CreatedAt) < 6 || e.CreatedAt[len(e.CreatedAt)-6:] != "+07:00" {
			t.Fatalf("created_at %q is not in WIB", e.CreatedAt)
		}
	}
	for _, want := range []key{
		{"visit_checked_in", f.guard.User.ID},
		{"visit_viewed", f.ownerA.User.ID},
		{"photo_viewed", f.ownerA.User.ID},
		{"visit_checked_out", f.guard.User.ID},
	} {
		if !got[want] {
			t.Errorf("no audit row for %+v on visit %s; have %v", want, v.ID, got)
		}
	}

	// Who and when are filterable; the actor's name comes back with the row.
	byGuard := fetchAudit(t, admin.AccessToken, url.Values{"user_id": {f.guard.User.ID}, "action": {"visit_checked_out"}})
	if len(byGuard.Data) != 1 || *byGuard.Data[0].UserName != "Guard AU" || byGuard.Data[0].Target != "visit:"+v.ID {
		t.Fatalf("by guard + action: %+v", byGuard.Data)
	}

	// History views by owners and admins are recorded with what they asked for; the guard's list is not.
	listed := fetchAudit(t, admin.AccessToken, url.Values{"action": {"visits_listed"}, "limit": {"200"}})
	seen := map[key]string{}
	for _, e := range listed.Data {
		seen[key{e.Action, *e.UserID}] = e.Target
	}
	if seen[key{"visits_listed", f.ownerA.User.ID}] != "house:"+f.houseA {
		t.Errorf("owner's listing target = %q, want their house", seen[key{"visits_listed", f.ownerA.User.ID}])
	}
	if _, ok := seen[key{"visits_listed", f.guard.User.ID}]; ok {
		t.Error("guard list polling must not be audited")
	}
	adminTargets := map[string]bool{}
	for _, e := range listed.Data {
		if *e.UserID == admin.User.ID {
			adminTargets[e.Target] = true
		}
	}
	if !adminTargets["house:"+f.houseA] || !adminTargets["houses:all"] {
		t.Errorf("admin listing targets = %v", adminTargets)
	}
	// A probe at another house's visit is not logged as a view of it.
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visit_viewed' AND target = $1`, "visit:"+f.visitB.ID); n != 0 {
		t.Errorf("refused lookup was logged as a view (%d rows)", n)
	}

	// A failed login for an unknown account has no actor but is still recorded.
	failed := fetchAudit(t, admin.AccessToken, url.Values{"action": {"login_failed"}, "target": {"unknown_email"}})
	if len(failed.Data) == 0 || failed.Data[0].UserID != nil || failed.Data[0].UserName != nil {
		t.Fatalf("unknown-email failure: %+v", failed.Data)
	}
}

func TestAuditLogFiltersAndPaging(t *testing.T) {
	f := newOwnerFixture(t, "AP")
	seed(t, "Admin AP", "admin-ap@example.test", "admin", nil)
	admin := login(t, "admin-ap@example.test")
	for i := 0; i < 4; i++ { // a few more rows for this guard
		assertStatus(t, do(t, "GET", "/v1/visits/"+f.visitsA[0].ID, f.guard.AccessToken, nil), http.StatusOK, "guard views")
	}

	want := count(t, `SELECT count(*) FROM audit_logs WHERE user_id = $1`, f.guard.User.ID)
	if want < 6 {
		t.Fatalf("setup: only %d rows for the guard", want)
	}
	var paged []int64
	seen := map[int64]bool{}
	cursor := ""
	for page := 0; page < 50; page++ {
		q := url.Values{"user_id": {f.guard.User.ID}, "limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		res := fetchAudit(t, admin.AccessToken, q)
		for _, e := range res.Data {
			if seen[e.ID] {
				t.Fatalf("row %d repeated across pages", e.ID)
			}
			seen[e.ID] = true
			paged = append(paged, e.ID)
			if *e.UserID != f.guard.User.ID {
				t.Fatalf("filter leaked another user's row: %+v", e)
			}
		}
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
	}
	if len(paged) != want {
		t.Fatalf("paged through %d rows, DB has %d", len(paged), want)
	}
	for i := 1; i < len(paged); i++ {
		if paged[i] >= paged[i-1] {
			t.Fatalf("pages are not strictly newest-first: %v", paged)
		}
	}

	// Date window in WIB: a range around today has rows, a range in the past has none.
	now := time.Now().In(wibZone)
	around := url.Values{"user_id": {f.guard.User.ID}, "limit": {"200"},
		"from": {now.AddDate(0, 0, -1).Format("2006-01-02")}, "to": {now.AddDate(0, 0, 1).Format("2006-01-02")}}
	if n := len(fetchAudit(t, admin.AccessToken, around).Data); n != want {
		t.Fatalf("rows around today = %d, want %d", n, want)
	}
	past := url.Values{"user_id": {f.guard.User.ID}, "from": {"2020-01-01"}, "to": {"2020-01-31"}}
	if n := len(fetchAudit(t, admin.AccessToken, past).Data); n != 0 {
		t.Fatalf("rows in 2020 = %d, want 0", n)
	}

	for name, q := range map[string]string{
		"bad user id":    "user_id=nope",
		"bad date":       "from=31-01-2026",
		"to before from": "from=2026-03-11&to=2026-03-10",
		"limit zero":     "limit=0",
		"limit too big":  "limit=201",
		"bad cursor":     "cursor=garbage",
		"long action":    "action=" + strings.Repeat("a", 65),
	} {
		assertStatus(t, do(t, "GET", "/v1/audit-logs?"+q, admin.AccessToken, nil), http.StatusUnprocessableEntity, name)
	}
}
