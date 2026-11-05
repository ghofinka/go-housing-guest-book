package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/notify"
	"github.com/ghofinka/go-housing-guest-book/internal/privacy"
	"github.com/ghofinka/go-housing-guest-book/internal/visit"
)

// fakeSender records the emails it is asked to send and can be told to fail.
type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
	// fail, when set, decides per message whether the send fails.
	fail func(m notify.Message, call int) error
	call int
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call++
	if f.fail != nil {
		if err := f.fail(m, f.call); err != nil {
			return err
		}
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeSender) to() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.To
	}
	slices.Sort(out)
	return out
}

func (f *fakeSender) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.call }

// clearOutbox sets aside the backlog other tests left, so a worker in this test only sees its own rows.
func clearOutbox(t *testing.T) {
	t.Helper()
	if _, err := owner.Exec(context.Background(),
		`UPDATE notification_outbox SET status = 'skipped', locked_until = NULL WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

type outboxRow struct {
	Email     string
	Status    string
	Attempts  int
	LastError string
	NextIn    time.Duration
}

func outboxFor(t *testing.T, visitID string) []outboxRow {
	t.Helper()
	rows, err := owner.Query(context.Background(), `
		SELECT u.email, o.status, o.attempts, coalesce(o.last_error, ''), o.next_attempt_at - now()
		FROM notification_outbox o JOIN users u ON u.id = o.user_id
		WHERE o.visit_id = $1 ORDER BY u.email`, visitID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.Email, &r.Status, &r.Attempts, &r.LastError, &r.NextIn); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func emails(rows []outboxRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Email
	}
	slices.Sort(out)
	return out
}

func setFastLane(t *testing.T, adminToken string, on bool) {
	t.Helper()
	assertStatus(t, do(t, "PUT", "/v1/settings/notifications", adminToken, map[string]any{"notify_fast_lane": on}), http.StatusOK, "set notify_fast_lane")
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM settings WHERE key = 'notify_fast_lane'`) })
}

type notifyFixture struct {
	houseA, houseB                      string
	guard, admin                        tokenPair
	ownerA1, ownerA2, ownerB, inactiveA string // email addresses
}

func newNotifyFixture(t *testing.T, tag string) notifyFixture {
	t.Helper()
	em := strings.ToLower(tag)
	f := notifyFixture{houseA: newHouse(t, tag, "A", true), houseB: newHouse(t, tag, "B", true)}
	f.ownerA1, f.ownerA2 = "owner-"+em+"-a1@example.test", "owner-"+em+"-a2@example.test"
	f.ownerB, f.inactiveA = "owner-"+em+"-b@example.test", "owner-"+em+"-a3@example.test"
	seed(t, "Owner "+tag+" A1", f.ownerA1, "owner", f.houseA)
	seed(t, "Owner "+tag+" A2", f.ownerA2, "owner", f.houseA)
	seed(t, "Owner "+tag+" A3", f.inactiveA, "owner", f.houseA)
	seed(t, "Owner "+tag+" B", f.ownerB, "owner", f.houseB)
	if _, err := owner.Exec(context.Background(), `UPDATE users SET active = false WHERE email = $1`, f.inactiveA); err != nil {
		t.Fatal(err)
	}
	seed(t, "Guard "+tag, "guard-"+em+"@example.test", "guard", nil)
	seed(t, "Admin "+tag, "admin-"+em+"@example.test", "admin", nil)
	f.guard, f.admin = login(t, "guard-"+em+"@example.test"), login(t, "admin-"+em+"@example.test")
	return f
}

func (f notifyFixture) checkIn(t *testing.T, house, name, category string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"house_id": house, "category": category}
	if name != "" {
		body["guest_name"] = name
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, body)
	assertStatus(t, rec, http.StatusCreated, "check-in "+category)
	return decode[visitBody](t, rec).ID
}

// S3: a check-in queues one email per active owner of the visit's house, in the same transaction.
func TestArrivalNotificationsAreQueuedForTheHouseOwners(t *testing.T) {
	f := newNotifyFixture(t, "NQ")
	clearOutbox(t)

	id := f.checkIn(t, f.houseA, "Zq Tamu", "tamu", nil)
	rows := outboxFor(t, id)
	if !slices.Equal(emails(rows), []string{f.ownerA1, f.ownerA2}) {
		t.Fatalf("queued for %v, want the two active owners of house A only (not the inactive one, not house B)", emails(rows))
	}
	for _, r := range rows {
		if r.Status != "pending" || r.Attempts != 0 {
			t.Fatalf("new row = %+v, want pending with no attempts", r)
		}
	}

	// A retry of the same check-in (same client_request_id) must not queue a second email.
	key := uuid.NewString()
	first := f.checkIn(t, f.houseB, "Zq Retry", "tamu", map[string]any{"client_request_id": key})
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken,
		map[string]any{"house_id": f.houseB, "guest_name": "Zq Retry", "category": "tamu", "client_request_id": key}), http.StatusOK, "replay")
	if got := outboxFor(t, first); len(got) != 1 || got[0].Email != f.ownerB {
		t.Fatalf("after a replay the queue for house B is %v, want exactly one row for the owner", emails(got))
	}

	// A check-in that is refused leaves nothing behind (the outbox row is part of the same transaction).
	before := count(t, `SELECT count(*) FROM notification_outbox`)
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken,
		map[string]any{"house_id": uuid.NewString(), "guest_name": "X", "category": "tamu"}), http.StatusUnprocessableEntity, "unknown house")
	assertStatus(t, do(t, "POST", "/v1/visits", f.guard.AccessToken,
		map[string]any{"house_id": f.houseA, "category": "tamu"}), http.StatusUnprocessableEntity, "missing name")
	if after := count(t, `SELECT count(*) FROM notification_outbox`); after != before {
		t.Fatalf("a refused check-in changed the outbox (%d -> %d rows)", before, after)
	}

	// A pre-registered visit notifies too.
	seed(t, "Owner NQ P", "owner-nq-p@example.test", "owner", f.houseA)
	ownerP := login(t, "owner-nq-p@example.test")
	pre := mustCreatePre(t, ownerP.AccessToken, "Zq Prereg", day(0), "")
	rec := do(t, "POST", "/v1/visits", f.guard.AccessToken, map[string]any{"pre_registration_code": pre.Code})
	assertStatus(t, rec, http.StatusCreated, "check-in with code")
	if got := outboxFor(t, decode[visitBody](t, rec).ID); len(got) != 3 {
		t.Fatalf("a pre-registered visit queued %d emails, want 3 (A1, A2 and the new owner)", len(got))
	}
}

func TestFastLaneNotificationsFollowTheAdminSetting(t *testing.T) {
	f := newNotifyFixture(t, "NF")
	clearOutbox(t)

	// Off by default: couriers and ojol drivers come all day and would flood the owners.
	for _, cat := range []string{"kurir", "ojol"} {
		if got := outboxFor(t, f.checkIn(t, f.houseA, "", cat, nil)); len(got) != 0 {
			t.Fatalf("%s queued %d emails with the setting off", cat, len(got))
		}
	}
	if got := outboxFor(t, f.checkIn(t, f.houseA, "Zq Tukang", "tukang", nil)); len(got) != 2 {
		t.Fatalf("a tukang visit queued %d emails, want 2", len(got))
	}

	setFastLane(t, f.admin.AccessToken, true)
	for _, cat := range []string{"kurir", "ojol"} {
		if got := outboxFor(t, f.checkIn(t, f.houseA, "", cat, nil)); len(got) != 2 {
			t.Fatalf("%s queued %d emails with the setting on, want 2", cat, len(got))
		}
	}
	setFastLane(t, f.admin.AccessToken, false)
	if got := outboxFor(t, f.checkIn(t, f.houseA, "", "kurir", nil)); len(got) != 0 {
		t.Fatalf("turning the setting off again still queued %d emails", len(got))
	}

	// A corrupt stored value counts as off.
	if _, err := owner.Exec(context.Background(), `INSERT INTO settings (key, value) VALUES ('notify_fast_lane', 'banana')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM settings WHERE key = 'notify_fast_lane'`) })
	if got := outboxFor(t, f.checkIn(t, f.houseA, "", "ojol", nil)); len(got) != 0 {
		t.Fatalf("a corrupt setting queued %d emails, want 0", len(got))
	}
}

func TestWorkerSendsAMinimalMessage(t *testing.T) {
	f := newNotifyFixture(t, "NW")
	clearOutbox(t)
	const secretName, secretPlate = "Zq Rahasia Besar", "B 9 ZQRAHAS"
	photo := map[string][]byte{"vehicle_photo": testJPEG(t, 60, 40, "")}
	rec := doForm(t, "/v1/visits", f.guard.AccessToken, map[string]string{
		"house_id": f.houseA, "guest_name": secretName, "category": "tukang", "vehicle_plate": secretPlate, "purpose_note": "Zq catatan rahasia"}, photo)
	assertStatus(t, rec, http.StatusCreated, "check-in")
	id := decode[visitBody](t, rec).ID

	s := &fakeSender{}
	stats := notify.NewWorker(app, s).Tick(context.Background())
	if stats.Sent != 2 || stats.Failed != 0 || stats.Retried != 0 {
		t.Fatalf("stats = %+v, want 2 sent", stats)
	}
	if !slices.Equal(s.to(), []string{f.ownerA1, f.ownerA2}) {
		t.Fatalf("sent to %v, want the two active owners of house A", s.to())
	}
	for _, m := range s.sent {
		whole := strings.ToLower(m.To + m.Subject + m.Body)
		for _, secret := range []string{secretName, secretPlate, "rahasia", "zq"} {
			if strings.Contains(whole, strings.ToLower(secret)) {
				t.Fatalf("the email contains guest data %q:\n%s", secret, m.Body)
			}
		}
		if !strings.Contains(m.Body, "(tukang)") || !strings.Contains(m.Body, "WIB") {
			t.Fatalf("the email lacks the category or time: %q", m.Body)
		}
	}
	for _, r := range outboxFor(t, id) {
		if r.Status != "sent" || r.Attempts != 1 || r.LastError != "" {
			t.Fatalf("row after sending = %+v, want sent on the first attempt", r)
		}
	}
	var sentAt *time.Time
	if err := owner.QueryRow(context.Background(), `SELECT sent_at FROM notification_outbox WHERE visit_id = $1 LIMIT 1`, id).Scan(&sentAt); err != nil || sentAt == nil {
		t.Fatalf("sent_at not set (%v)", err)
	}
	// Nothing is left to send, and sending is not audited per email (counts are logged instead).
	if again := notify.NewWorker(app, s).Tick(context.Background()); again != (notify.Stats{}) {
		t.Fatalf("a second tick did something: %+v", again)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action LIKE 'notification%' AND action <> 'notification_settings_changed'`); n != 0 {
		t.Fatalf("%d audit rows for individual emails, want 0", n)
	}
}

func TestWorkerRetriesWithBackoffThenGivesUp(t *testing.T) {
	f := newNotifyFixture(t, "NR")
	clearOutbox(t)
	id := f.checkIn(t, f.houseB, "Zq Gagal", "tamu", nil) // house B has a single owner

	// The server's reply quotes the address; it must never be stored.
	s := &fakeSender{fail: func(m notify.Message, _ int) error {
		return fmt.Errorf("550 5.1.1 <%s>: user unknown", m.To)
	}}
	w := notify.NewWorker(app, s)

	if got := w.Tick(context.Background()); got.Retried != 1 || got.Sent != 0 {
		t.Fatalf("first tick = %+v, want 1 retried", got)
	}
	row := outboxFor(t, id)[0]
	if row.Status != "pending" || row.Attempts != 1 || row.NextIn < 50*time.Second || row.NextIn > 70*time.Second {
		t.Fatalf("after attempt 1: %+v, want pending, 1 attempt, retry in about 1 minute", row)
	}
	if row.LastError != "send error" || strings.Contains(row.LastError, f.ownerB) {
		t.Fatalf("last_error = %q, want a short reason without the address", row.LastError)
	}

	// Nothing is due yet, so an immediate tick does not call the sender again.
	calls := s.calls()
	if got := w.Tick(context.Background()); got != (notify.Stats{}) || s.calls() != calls {
		t.Fatalf("a tick before the backoff elapsed sent again (stats %+v, calls %d -> %d)", got, calls, s.calls())
	}

	// Attempts 2 to 4 back off by 2, 4 and 8 minutes; the 5th failure is final.
	wantNext := map[int]time.Duration{2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute}
	for attempt := 2; attempt <= 5; attempt++ {
		if _, err := owner.Exec(context.Background(), `UPDATE notification_outbox SET next_attempt_at = now() - interval '1 second' WHERE visit_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		w.Tick(context.Background())
		row = outboxFor(t, id)[0]
		if row.Attempts != attempt {
			t.Fatalf("attempts = %d after round %d", row.Attempts, attempt)
		}
		if attempt < 5 {
			want := wantNext[attempt]
			if row.Status != "pending" || row.NextIn < want-10*time.Second || row.NextIn > want+10*time.Second {
				t.Fatalf("after attempt %d: %+v, want pending and a retry in about %v", attempt, row, want)
			}
		}
	}
	if row.Status != "failed" || row.Attempts != notify.MaxAttempts {
		t.Fatalf("after %d failures: %+v, want failed", notify.MaxAttempts, row)
	}
	calls = s.calls()
	if _, err := owner.Exec(context.Background(), `UPDATE notification_outbox SET next_attempt_at = now() - interval '1 second' WHERE visit_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	w.Tick(context.Background())
	if s.calls() != calls {
		t.Fatal("a failed notification was tried again")
	}

	// A structured SMTP reply is summarised by its code.
	id2 := f.checkIn(t, f.houseB, "Zq Kode", "tamu", nil)
	s2 := &fakeSender{fail: func(notify.Message, int) error {
		return &textproto.Error{Code: 550, Msg: "mailbox for " + f.ownerB + " unavailable"}
	}}
	notify.NewWorker(app, s2).Tick(context.Background())
	if got := outboxFor(t, id2)[0].LastError; got != "smtp reply 550" {
		t.Fatalf("last_error = %q, want %q", got, "smtp reply 550")
	}

	// It can recover: two failures and then a success ends as sent, with the error cleared.
	id3 := f.checkIn(t, f.houseB, "Zq Pulih", "tamu", nil)
	s3 := &fakeSender{fail: func(_ notify.Message, call int) error {
		if call <= 2 {
			return errors.New("temporary")
		}
		return nil
	}}
	w3 := notify.NewWorker(app, s3)
	for i := 0; i < 3; i++ {
		if _, err := owner.Exec(context.Background(), `UPDATE notification_outbox SET next_attempt_at = now() - interval '1 second' WHERE visit_id = $1`, id3); err != nil {
			t.Fatal(err)
		}
		w3.Tick(context.Background())
	}
	if row := outboxFor(t, id3)[0]; row.Status != "sent" || row.Attempts != 3 || row.LastError != "" {
		t.Fatalf("after recovering: %+v, want sent on attempt 3 with the error cleared", row)
	}
}

func TestWorkerSkipsARecipientWhoWasDeactivated(t *testing.T) {
	f := newNotifyFixture(t, "NS")
	clearOutbox(t)
	id := f.checkIn(t, f.houseB, "Zq Nonaktif", "tamu", nil)
	if _, err := owner.Exec(context.Background(), `UPDATE users SET active = false WHERE email = $1`, f.ownerB); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{}
	if got := notify.NewWorker(app, s).Tick(context.Background()); got.Skipped != 1 || got.Sent != 0 {
		t.Fatalf("stats = %+v, want 1 skipped", got)
	}
	if s.calls() != 0 {
		t.Fatal("an email was sent to a deactivated account")
	}
	if row := outboxFor(t, id)[0]; row.Status != "skipped" {
		t.Fatalf("row = %+v, want skipped", row)
	}
}

func TestWorkersNeverSendTheSameNotificationTwice(t *testing.T) {
	f := newNotifyFixture(t, "NC")
	clearOutbox(t)
	const visits = 12
	var ids []string
	for i := 0; i < visits; i++ {
		ids = append(ids, f.checkIn(t, f.houseB, fmt.Sprintf("Zq Paralel %d", i), "tamu", nil))
	}

	s := &fakeSender{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := notify.NewWorker(app, s).WithBatchSize(2) // small batches so the workers really compete
			for i := 0; i < 8; i++ {
				worker.Tick(context.Background())
			}
		}()
	}
	wg.Wait()

	if got := s.calls(); got != visits {
		t.Fatalf("%d sends for %d notifications: some were sent twice or not at all", got, visits)
	}
	for _, id := range ids {
		if r := outboxFor(t, id)[0]; r.Status != "sent" || r.Attempts != 1 {
			t.Fatalf("row %+v, want sent exactly once", r)
		}
	}
}

func TestAClaimedNotificationIsNotTakenByAnotherWorkerUntilItExpires(t *testing.T) {
	f := newNotifyFixture(t, "NL")
	clearOutbox(t)
	id := f.checkIn(t, f.houseB, "Zq Dikunci", "tamu", nil)
	// Another worker holds the row mid-send.
	if _, err := owner.Exec(context.Background(), `UPDATE notification_outbox SET locked_until = now() + interval '2 minutes' WHERE visit_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{}
	if got := notify.NewWorker(app, s).Tick(context.Background()); got != (notify.Stats{}) {
		t.Fatalf("a claimed row was taken: %+v", got)
	}
	// That worker crashed: its claim runs out and the row is sent by the next one.
	if _, err := owner.Exec(context.Background(), `UPDATE notification_outbox SET locked_until = now() - interval '1 second' WHERE visit_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if got := notify.NewWorker(app, s).Tick(context.Background()); got.Sent != 1 {
		t.Fatalf("an expired claim was not picked up: %+v", got)
	}
}

func TestWorkerDeletesOldFinishedRowsOnly(t *testing.T) {
	f := newNotifyFixture(t, "ND")
	clearOutbox(t)
	id := f.checkIn(t, f.houseA, "Zq Lama", "tamu", nil) // two rows: A1 and A2
	ctx := context.Background()
	var a1, a2 int64
	if err := owner.QueryRow(ctx, `SELECT o.id FROM notification_outbox o JOIN users u ON u.id = o.user_id WHERE o.visit_id = $1 AND u.email = $2`, id, f.ownerA1).Scan(&a1); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT o.id FROM notification_outbox o JOIN users u ON u.id = o.user_id WHERE o.visit_id = $1 AND u.email = $2`, id, f.ownerA2).Scan(&a2); err != nil {
		t.Fatal(err)
	}
	// A1: finished 31 days ago. A2: still pending and 31 days old (it must survive; the sender fails
	// so it stays pending).
	if _, err := owner.Exec(ctx, `UPDATE notification_outbox SET status = 'sent', created_at = now() - interval '31 days' WHERE id = $1`, a1); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE notification_outbox SET created_at = now() - interval '31 days' WHERE id = $1`, a2); err != nil {
		t.Fatal(err)
	}
	// A recent finished row stays as well.
	recent := f.checkIn(t, f.houseB, "Zq Baru", "tamu", nil)
	if _, err := owner.Exec(ctx, `UPDATE notification_outbox SET status = 'failed' WHERE visit_id = $1`, recent); err != nil {
		t.Fatal(err)
	}

	s := &fakeSender{fail: func(notify.Message, int) error { return errors.New("down") }}
	notify.NewWorker(app, s).Tick(ctx) // a new worker cleans up on its first tick

	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE id = $1`, a1); n != 0 {
		t.Fatal("a finished row older than 30 days was kept")
	}
	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE id = $1 AND status = 'pending'`, a2); n != 1 {
		t.Fatal("a pending row was deleted or sent")
	}
	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE visit_id = $1 AND status = 'failed'`, recent); n != 1 {
		t.Fatal("a recent finished row was deleted")
	}
}

func TestNoNotificationsWithoutAMailServer(t *testing.T) {
	f := newNotifyFixture(t, "NO")
	clearOutbox(t)
	// A visit service built without WithNotifications is what runs when SMTP_HOST is not set.
	svc := visit.NewService(app, photoStore)
	guardID, err := uuid.Parse(f.guard.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := "Zq Tanpa Email"
	v, created, err := svc.CheckIn(context.Background(), auth.Principal{UserID: guardID, Role: auth.RoleGuard},
		visit.CheckInInput{HouseID: uuid.MustParse(f.houseA), GuestName: &name, Category: "tamu"})
	if err != nil || !created {
		t.Fatalf("check-in: created=%v err=%v", created, err)
	}
	if n := count(t, `SELECT count(*) FROM notification_outbox WHERE visit_id = $1`, v.ID.String()); n != 0 {
		t.Fatalf("%d notifications were queued with no mail server configured", n)
	}
}

func TestNotificationSettingsEndpoint(t *testing.T) {
	f := newNotifyFixture(t, "NE")
	clearOutbox(t)
	seed(t, "Owner NE", "owner-ne-x@example.test", "owner", f.houseB)
	ownerTok := login(t, "owner-ne-x@example.test")
	f.checkIn(t, f.houseB, "Zq Antrean", "tamu", nil)

	type status struct {
		NotifyFastLane bool `json:"notify_fast_lane"`
		Enabled        bool `json:"enabled"`
		Pending        int  `json:"pending"`
		Sent           int  `json:"sent"`
		Failed         int  `json:"failed"`
		Skipped        int  `json:"skipped"`
	}
	got := decode[status](t, do(t, "GET", "/v1/settings/notifications", f.admin.AccessToken, nil))
	if got.NotifyFastLane || !got.Enabled {
		t.Fatalf("defaults = %+v, want fast lane off and notifications enabled", got)
	}
	if want := count(t, `SELECT count(*) FROM notification_outbox WHERE status = 'pending'`); got.Pending != want || got.Pending < 2 {
		t.Fatalf("pending = %d, database says %d (expected at least the 2 just queued)", got.Pending, want)
	}

	put := decode[status](t, do(t, "PUT", "/v1/settings/notifications", f.admin.AccessToken, map[string]any{"notify_fast_lane": true}))
	t.Cleanup(func() { _, _ = owner.Exec(context.Background(), `DELETE FROM settings WHERE key = 'notify_fast_lane'`) })
	if !put.NotifyFastLane {
		t.Fatalf("the update did not stick: %+v", put)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'notification_settings_changed' AND target = 'notify_fast_lane:false->true'`); n == 0 {
		t.Fatal("the change was not audited with the old and new value")
	}

	assertStatus(t, do(t, "PUT", "/v1/settings/notifications", f.admin.AccessToken, map[string]any{}), http.StatusUnprocessableEntity, "missing field")
	assertStatus(t, do(t, "PUT", "/v1/settings/notifications", f.admin.AccessToken, map[string]any{"notify_fast_lane": "yes"}), http.StatusBadRequest, "wrong type")
	for name, tok := range map[string]string{"guard": f.guard.AccessToken, "owner": ownerTok.AccessToken} {
		assertStatus(t, do(t, "GET", "/v1/settings/notifications", tok, nil), http.StatusForbidden, name+" read")
		assertStatus(t, do(t, "PUT", "/v1/settings/notifications", tok, map[string]any{"notify_fast_lane": true}), http.StatusForbidden, name+" write")
	}
	assertStatus(t, do(t, "GET", "/v1/settings/notifications", "", nil), http.StatusUnauthorized, "anonymous read")
}

// The public notice must say that owners are emailed, but only when they are.
func TestPrivacyNoticeDisclosesOwnerEmailOnlyWhenOn(t *testing.T) {
	ctx := context.Background()
	off, err := privacy.NewService(app).Notice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	on, err := privacy.NewService(app, privacy.WithOwnerEmail()).Notice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	headings := func(n privacy.Notice) []string {
		out := make([]string, len(n.Sections))
		for i, s := range n.Sections {
			out[i] = s.Heading
		}
		return out
	}
	const section = "Pemberitahuan ke pemilik rumah"
	if slices.Contains(headings(off), section) {
		t.Fatal("the notice mentions owner email although it is off")
	}
	idx := slices.Index(headings(on), section)
	if idx != slices.Index(headings(on), "Siapa yang dapat melihat")+1 {
		t.Fatalf("section order = %v, want the email section right after who can see the data", headings(on))
	}
	if len(on.Sections) != len(off.Sections)+1 {
		t.Fatalf("%d sections with email, %d without, want one more", len(on.Sections), len(off.Sections))
	}
	text := strings.Join(on.Sections[idx].Items, " ")
	for _, want := range []string{"email", "tanpa nama", "tanpa plat nomor", "di luar sistem"} {
		if !strings.Contains(text, want) {
			t.Errorf("the email section does not say %q: %s", want, text)
		}
	}
	purpose := func(n privacy.Notice) string {
		return strings.Join(n.Sections[slices.Index(headings(n), "Untuk apa")].Items, " ")
	}
	if !strings.Contains(purpose(off), "Data tidak dipakai untuk tujuan lain.") {
		t.Fatalf("the original promise changed: %s", purpose(off))
	}
	if strings.Contains(purpose(on), "tujuan lain.") || !strings.Contains(purpose(on), "selain memberi tahu pemilik rumah") {
		t.Fatalf("the promise of no other use must name the owner email: %s", purpose(on))
	}
}
