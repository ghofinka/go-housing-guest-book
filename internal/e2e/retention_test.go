package e2e

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/photo"
	"github.com/ghofinka/go-housing-guest-book/internal/retention"
)

// fullVisit is a visit with every field the retention and erasure tests look at.
type fullVisit struct {
	ID              string  `json:"id"`
	GuestName       *string `json:"guest_name"`
	PurposeNote     *string `json:"purpose_note"`
	VehiclePlate    *string `json:"vehicle_plate"`
	Category        string  `json:"category"`
	CheckinAt       string  `json:"checkin_at"`
	Status          string  `json:"status"`
	Anonymized      bool    `json:"anonymized"`
	VehiclePhotoURL *string `json:"vehicle_photo_url"`
	FacePhotoURL    *string `json:"face_photo_url"`
	House           struct {
		ID string `json:"id"`
	} `json:"house"`
}

type runResult struct {
	RunID             int64 `json:"run_id"`
	DryRun            bool  `json:"dry_run"`
	PhotosDeleted     int   `json:"photos_deleted"`
	RecordsAnonymized int   `json:"records_anonymized"`
	FilesFailed       int   `json:"files_failed"`
}

// resetRetentionSettings puts the retention limits back to the defaults now and after the test.
func resetRetentionSettings(t *testing.T) {
	t.Helper()
	set := func() {
		for k, v := range map[string]string{"photo_retention_days": "30", "personal_data_retention_days": "90"} {
			if _, err := owner.Exec(context.Background(), `UPDATE settings SET value = $2, updated_at = now() WHERE key = $1`, k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	set()
	t.Cleanup(set)
}

// makeVisit checks a guest in (with a vehicle photo, and a face photo if asked) and then
// ages the record by ageDays. Name, plate and note are all filled in.
func makeVisit(t *testing.T, guardToken, houseID, name string, face bool, ageDays int) fullVisit {
	t.Helper()
	files := map[string][]byte{"vehicle_photo": testJPEG(t, 80, 60, "")}
	if face {
		files["face_photo"] = testJPEG(t, 50, 50, "")
	}
	rec := doForm(t, "/v1/visits", guardToken, map[string]string{
		"house_id": houseID, "guest_name": name, "category": "tamu",
		"purpose_note": "Catatan " + name, "vehicle_plate": "B 1234 RT",
	}, files)
	assertStatus(t, rec, http.StatusCreated, "check-in "+name)
	v := decode[fullVisit](t, rec)
	if ageDays > 0 {
		if _, err := owner.Exec(context.Background(),
			`UPDATE visits SET checkin_at = now() - make_interval(days => $2) WHERE id = $1`, v.ID, ageDays); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func runRetention(t *testing.T, adminToken, query string) runResult {
	t.Helper()
	rec := do(t, "POST", "/v1/retention/run"+query, adminToken, nil)
	assertStatus(t, rec, http.StatusOK, "retention run "+query)
	return decode[runResult](t, rec)
}

func getVisit(t *testing.T, bearer, id string) fullVisit {
	t.Helper()
	rec := do(t, "GET", "/v1/visits/"+id, bearer, nil)
	assertStatus(t, rec, http.StatusOK, "get visit")
	return decode[fullVisit](t, rec)
}

func photoPaths(t *testing.T, id string) (vehicle, face *string) {
	t.Helper()
	if err := owner.QueryRow(context.Background(),
		`SELECT vehicle_photo_path, face_photo_path FROM visits WHERE id = $1`, id).Scan(&vehicle, &face); err != nil {
		t.Fatal(err)
	}
	return
}

// retentionStatus is the body of GET /v1/settings/retention.
type retentionStatus struct {
	PhotoDays  int `json:"photo_retention_days"`
	PersonDays int `json:"personal_data_retention_days"`
	RecentRuns []struct {
		ID                int64   `json:"id"`
		Trigger           string  `json:"trigger"`
		DryRun            bool    `json:"dry_run"`
		PhotosDeleted     int     `json:"photos_deleted"`
		RecordsAnonymized int     `json:"records_anonymized"`
		FinishedAt        *string `json:"finished_at"`
		Error             *string `json:"error"`
	} `json:"recent_runs"`
}

func TestRetentionPurgesByAge(t *testing.T) {
	resetRetentionSettings(t)
	seed(t, "Admin RT", "admin-rt@example.test", "admin", nil)
	seed(t, "Guard RT", "guard-rt@example.test", "guard", nil)
	admin, guard := login(t, "admin-rt@example.test"), login(t, "guard-rt@example.test")
	houseID := newHouse(t, "RT", "1", true)

	runRetention(t, admin.AccessToken, "") // clear anything old left by earlier tests so the counts below are exact

	v29 := makeVisit(t, guard.AccessToken, houseID, "Umur 29", false, 29)
	v31 := makeVisit(t, guard.AccessToken, houseID, "Umur 31", true, 31) // vehicle + face photo
	v89 := makeVisit(t, guard.AccessToken, houseID, "Umur 89", false, 89)
	v91 := makeVisit(t, guard.AccessToken, houseID, "Umur 91", false, 91)
	vInside := makeVisit(t, guard.AccessToken, houseID, "Masih di dalam", false, 100) // never checked out
	vNew := makeVisit(t, guard.AccessToken, houseID, "Baru", false, 0)
	files0 := photoFiles(t)

	// Dry run: reports what would happen and changes nothing.
	dry := runRetention(t, admin.AccessToken, "?dry_run=true")
	if !dry.DryRun || dry.PhotosDeleted != 5 || dry.RecordsAnonymized != 2 {
		t.Fatalf("dry run = %+v, want 5 photos and 2 records", dry)
	}
	if photoFiles(t) != files0 || getVisit(t, admin.AccessToken, v91.ID).Anonymized {
		t.Fatal("a dry run changed data")
	}

	// Real run.
	res := runRetention(t, admin.AccessToken, "")
	if res.DryRun || res.PhotosDeleted != 5 || res.RecordsAnonymized != 2 || res.FilesFailed != 0 {
		t.Fatalf("run = %+v, want 5 photos, 2 records, 0 failed", res)
	}
	if got := files0 - photoFiles(t); got != 5 {
		t.Fatalf("%d files removed from disk, want 5", got)
	}

	// Photos: gone past 30 days, kept before it.
	for _, tc := range []struct {
		v        fullVisit
		wantKept bool
	}{{v29, true}, {v31, false}, {v89, false}, {v91, false}, {vInside, false}, {vNew, true}} {
		veh, face := photoPaths(t, tc.v.ID)
		if tc.wantKept != (veh != nil) {
			t.Errorf("%s: vehicle photo kept = %v, want %v", *tc.v.GuestName, veh != nil, tc.wantKept)
		}
		if face != nil {
			t.Errorf("%s: face photo path should be empty", *tc.v.GuestName)
		}
		wantStatus := http.StatusNotFound
		if tc.wantKept {
			wantStatus = http.StatusOK
		}
		assertStatus(t, do(t, "GET", "/v1/visits/"+tc.v.ID+"/photos/vehicle", admin.AccessToken, nil), wantStatus, "photo of "+*tc.v.GuestName)
	}

	// Personal text: intact at 29/31/89 days, blanked from 91 days (even for a visit that never checked out).
	for _, v := range []fullVisit{v29, v31, v89, vNew} {
		got := getVisit(t, admin.AccessToken, v.ID)
		if got.Anonymized || got.GuestName == nil || *got.GuestName != *v.GuestName || got.VehiclePlate == nil || got.PurposeNote == nil {
			t.Errorf("%s was anonymized too early: %+v", *v.GuestName, got)
		}
	}
	for _, v := range []fullVisit{v91, vInside} {
		got := getVisit(t, admin.AccessToken, v.ID)
		if !got.Anonymized || got.GuestName != nil || got.VehiclePlate != nil || got.PurposeNote != nil {
			t.Errorf("%s was not anonymized: %+v", *v.GuestName, got)
		}
		// What statistics need is untouched.
		if got.Category != "tamu" || got.House.ID != houseID || got.CheckinAt == "" || got.CheckinAt != getVisit(t, admin.AccessToken, v.ID).CheckinAt {
			t.Errorf("anonymizing changed non-personal fields: %+v", got)
		}
		if got.VehiclePhotoURL != nil || got.FacePhotoURL != nil {
			t.Errorf("photo urls survive anonymization: %+v", got)
		}
	}
	if got := getVisit(t, admin.AccessToken, vInside.ID); got.Status != "inside" {
		t.Errorf("retention must not check a guest out: status = %s", got.Status)
	}
	if n := count(t, `SELECT count(*) FROM visits WHERE id = ANY($1) AND anonymized_at IS NOT NULL`, []string{v91.ID, vInside.ID}); n != 2 {
		t.Errorf("anonymized_at set on %d rows, want 2", n)
	}

	// Idempotent: nothing more to do.
	if again := runRetention(t, admin.AccessToken, ""); again.PhotosDeleted != 0 || again.RecordsAnonymized != 0 {
		t.Fatalf("second run did work: %+v", again)
	}

	// The run is recorded in the history and in the audit log, with the admin as actor.
	status := decode[retentionStatus](t, do(t, "GET", "/v1/settings/retention", admin.AccessToken, nil))
	if status.PhotoDays != 30 || status.PersonDays != 90 || len(status.RecentRuns) < 3 {
		t.Fatalf("status = %+v", status)
	}
	var realRun, dryRun bool
	for _, r := range status.RecentRuns {
		if r.ID == res.RunID && r.Trigger == "manual" && !r.DryRun && r.PhotosDeleted == 5 && r.RecordsAnonymized == 2 && r.FinishedAt != nil && r.Error == nil {
			realRun = true
		}
		if r.ID == dry.RunID && r.DryRun {
			dryRun = true
		}
	}
	if !realRun || !dryRun {
		t.Fatalf("run history does not show both runs: %+v", status.RecentRuns)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'retention_run' AND user_id = $1 AND starts_with(target, $2)`,
		admin.User.ID, "run:"+strconv.FormatInt(res.RunID, 10)+" "); n != 1 {
		t.Fatalf("retention_run audit rows for the run = %d, want 1", n)
	}
	for _, text := range []string{"Umur 91", "B 1234 RT"} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE target LIKE '%' || $1 || '%'`, text); n != 0 {
			t.Fatalf("personal data %q leaked into the audit log", text)
		}
	}
}

func TestRetentionSettings(t *testing.T) {
	resetRetentionSettings(t)
	seed(t, "Admin RS", "admin-rs@example.test", "admin", nil)
	seed(t, "Guard RS", "guard-rs@example.test", "guard", nil)
	houseID := newHouse(t, "RS", "1", true)
	seed(t, "Owner RS", "owner-rs@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-rs@example.test"), login(t, "guard-rs@example.test"), login(t, "owner-rs@example.test")

	// Admin only.
	assertStatus(t, do(t, "GET", "/v1/settings/retention", "", nil), http.StatusUnauthorized, "anonymous get")
	for name, tok := range map[string]string{"guard": guard.AccessToken, "owner": own.AccessToken} {
		assertStatus(t, do(t, "GET", "/v1/settings/retention", tok, nil), http.StatusForbidden, name+" get")
		assertStatus(t, do(t, "PUT", "/v1/settings/retention", tok, map[string]int{"photo_retention_days": 7, "personal_data_retention_days": 7}), http.StatusForbidden, name+" put")
		assertStatus(t, do(t, "POST", "/v1/retention/run", tok, nil), http.StatusForbidden, name+" run")
	}

	for name, body := range map[string]map[string]any{
		"photo zero":            {"photo_retention_days": 0, "personal_data_retention_days": 90},
		"photo over a year":     {"photo_retention_days": 366, "personal_data_retention_days": 1095},
		"personal zero":         {"photo_retention_days": 30, "personal_data_retention_days": 0},
		"personal too long":     {"photo_retention_days": 30, "personal_data_retention_days": 1096},
		"photo outlives record": {"photo_retention_days": 91, "personal_data_retention_days": 90},
		"missing personal":      {"photo_retention_days": 30},
		"missing photo":         {"personal_data_retention_days": 90},
		"unknown field":         {"photo_retention_days": 30, "personal_data_retention_days": 90, "keep_forever": true},
	} {
		code := do(t, "PUT", "/v1/settings/retention", admin.AccessToken, body).Code
		want := http.StatusUnprocessableEntity
		if name == "unknown field" {
			want = http.StatusBadRequest
		}
		if code != want {
			t.Errorf("%s: status = %d, want %d", name, code, want)
		}
	}
	if n := count(t, `SELECT count(*) FROM settings WHERE key = 'photo_retention_days' AND value = '30'`); n != 1 {
		t.Fatal("a rejected update changed the stored value")
	}

	// A valid change is stored, returned and audited with old and new values (no personal data involved).
	rec := do(t, "PUT", "/v1/settings/retention", admin.AccessToken, map[string]int{"photo_retention_days": 10, "personal_data_retention_days": 45})
	assertStatus(t, rec, http.StatusOK, "valid update")
	got := decode[struct {
		Photo    int `json:"photo_retention_days"`
		Personal int `json:"personal_data_retention_days"`
	}](t, rec)
	if got.Photo != 10 || got.Personal != 45 {
		t.Fatalf("returned %+v", got)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'retention_settings_changed' AND user_id = $1 AND target = $2`,
		admin.User.ID, "photo_retention_days:30->10,personal_data_retention_days:90->45"); n != 1 {
		t.Fatalf("settings change audit rows = %d, want 1", n)
	}

	// The new limits are what the next run uses: a 15-day-old photo is now past the limit (10), a 5-day-old one is not.
	runRetention(t, admin.AccessToken, "") // clear old rows from other tests
	vOld := makeVisit(t, guard.AccessToken, houseID, "Lima belas hari", false, 15)
	vYoung := makeVisit(t, guard.AccessToken, houseID, "Lima hari", false, 5)
	dry := runRetention(t, admin.AccessToken, "?dry_run=true")
	if dry.PhotosDeleted != 1 || dry.RecordsAnonymized != 0 {
		t.Fatalf("with 10/45 days a dry run reports %+v, want 1 photo and 0 records", dry)
	}
	runRetention(t, admin.AccessToken, "")
	if veh, _ := photoPaths(t, vOld.ID); veh != nil {
		t.Error("15-day-old photo should be purged under a 10-day limit")
	}
	if veh, _ := photoPaths(t, vYoung.ID); veh == nil {
		t.Error("5-day-old photo should be kept under a 10-day limit")
	}
}

// flakyStore fails to delete selected keys, like a locked or read-only file would.
type flakyStore struct {
	photo.Store
	failKeys map[string]bool
}

func (f *flakyStore) Delete(ctx context.Context, key string) error {
	if f.failKeys[key] {
		return errors.New("simulated disk error")
	}
	return f.Store.Delete(ctx, key)
}

func TestRetentionKeepsTrackOfFilesItCouldNotDelete(t *testing.T) {
	resetRetentionSettings(t)
	seed(t, "Admin RF", "admin-rf@example.test", "admin", nil)
	seed(t, "Guard RF", "guard-rf@example.test", "guard", nil)
	admin, guard := login(t, "admin-rf@example.test"), login(t, "guard-rf@example.test")
	houseID := newHouse(t, "RF", "1", true)
	runRetention(t, admin.AccessToken, "")

	stuck := makeVisit(t, guard.AccessToken, houseID, "Berkas macet", false, 40)
	fine := makeVisit(t, guard.AccessToken, houseID, "Berkas biasa", false, 41)
	stuckPath, _ := photoPaths(t, stuck.ID)
	files0 := photoFiles(t)

	store := &flakyStore{Store: photoStore, failKeys: map[string]bool{*stuckPath: true}}
	svc := retention.NewService(app, store)
	ctx := context.Background()

	res, err := svc.Run(ctx, retention.TriggerCLI, nil, false)
	if err != nil {
		t.Fatalf("a file that cannot be deleted must not abort the run: %v", err)
	}
	if res.FilesFailed != 1 || res.PhotosDeleted != 1 {
		t.Fatalf("run = %+v, want 1 deleted and 1 failed", res)
	}
	// The healthy one is gone; the stuck one keeps its path so nothing is forgotten.
	if veh, _ := photoPaths(t, fine.ID); veh != nil {
		t.Error("the other photo should have been purged despite the failure")
	}
	if veh, _ := photoPaths(t, stuck.ID); veh == nil || *veh != *stuckPath {
		t.Fatal("the path of a file that could not be deleted was cleared")
	}
	if photoFiles(t) != files0-1 {
		t.Fatalf("files on disk = %d, want %d", photoFiles(t), files0-1)
	}
	var errText *string
	if err := owner.QueryRow(ctx, `SELECT error FROM retention_runs WHERE id = $1`, res.RunID).Scan(&errText); err != nil || errText == nil || !strings.Contains(*errText, "could not be deleted") {
		t.Fatalf("run history should note the failure: %v %v", errText, err)
	}

	// Next run, disk fixed: it is retried and succeeds.
	store.failKeys = nil
	res2, err := svc.Run(ctx, retention.TriggerCLI, nil, false)
	if err != nil || res2.PhotosDeleted != 1 || res2.FilesFailed != 0 {
		t.Fatalf("retry = %+v, %v", res2, err)
	}
	if veh, _ := photoPaths(t, stuck.ID); veh != nil || photoFiles(t) != files0-2 {
		t.Fatal("the stuck photo was not purged on retry")
	}
}

func TestRetentionRunsOneAtATime(t *testing.T) {
	resetRetentionSettings(t)
	seed(t, "Admin RL", "admin-rl@example.test", "admin", nil)
	admin := login(t, "admin-rl@example.test")

	// Pretend another instance (or the CLI) is mid-run by holding the lock from a different session.
	ctx := context.Background()
	conn, err := owner.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, retention.LockKey).Scan(&got); err != nil || !got {
		t.Fatalf("could not take the lock: %v %v", got, err)
	}
	rec := do(t, "POST", "/v1/retention/run", admin.AccessToken, nil)
	assertStatus(t, rec, http.StatusConflict, "run while another is in progress")
	if !strings.Contains(rec.Body.String(), "retention_running") {
		t.Fatalf("body = %s", rec.Body)
	}
	if _, err := retention.NewService(app, photoStore).Run(ctx, retention.TriggerSchedule, nil, false); !errors.Is(err, retention.ErrAlreadyRunning) {
		t.Fatalf("direct run = %v, want ErrAlreadyRunning", err)
	}

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, retention.LockKey); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	runRetention(t, admin.AccessToken, "") // works again once the lock is free
}

func TestEraseOnRequest(t *testing.T) {
	resetRetentionSettings(t)
	seed(t, "Admin ER", "admin-er@example.test", "admin", nil)
	seed(t, "Guard ER", "guard-er@example.test", "guard", nil)
	houseID := newHouse(t, "ER", "1", true)
	seed(t, "Owner ER", "owner-er@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-er@example.test"), login(t, "guard-er@example.test"), login(t, "owner-er@example.test")

	v := makeVisit(t, guard.AccessToken, houseID, "Minta dihapus", true, 3) // recent: retention would not touch it
	other := makeVisit(t, guard.AccessToken, houseID, "Tidak dihapus", false, 3)
	url := "/v1/visits/" + v.ID + "/personal-data"
	files0 := photoFiles(t)

	// Only admins may erase.
	assertStatus(t, do(t, "DELETE", url, "", nil), http.StatusUnauthorized, "anonymous")
	assertStatus(t, do(t, "DELETE", url, guard.AccessToken, nil), http.StatusForbidden, "guard")
	assertStatus(t, do(t, "DELETE", url, own.AccessToken, nil), http.StatusForbidden, "owner, even of that house")
	assertStatus(t, do(t, "DELETE", "/v1/visits/"+uuid.NewString()+"/personal-data", admin.AccessToken, nil), http.StatusNotFound, "unknown visit")
	assertStatus(t, do(t, "DELETE", "/v1/visits/nope/personal-data", admin.AccessToken, nil), http.StatusNotFound, "malformed id")
	if getVisit(t, admin.AccessToken, v.ID).Anonymized {
		t.Fatal("refused requests must not erase anything")
	}

	rec := do(t, "DELETE", url, admin.AccessToken, nil)
	assertStatus(t, rec, http.StatusOK, "erase")
	got := decode[fullVisit](t, rec)
	if !got.Anonymized || got.GuestName != nil || got.VehiclePlate != nil || got.PurposeNote != nil || got.VehiclePhotoURL != nil || got.FacePhotoURL != nil {
		t.Fatalf("erased visit still shows data: %+v", got)
	}
	if got.Category != "tamu" || got.House.ID != houseID || got.CheckinAt == "" {
		t.Fatalf("the record itself should stay: %+v", got)
	}
	if photoFiles(t) != files0-2 {
		t.Fatalf("files on disk = %d, want %d (both photos removed)", photoFiles(t), files0-2)
	}
	if veh, face := photoPaths(t, v.ID); veh != nil || face != nil {
		t.Fatal("photo paths remain in the database")
	}
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID+"/photos/vehicle", admin.AccessToken, nil), http.StatusNotFound, "photo after erasure")
	// The owner's history shows an anonymized entry, not the person's data.
	if o := getVisit(t, own.AccessToken, v.ID); !o.Anonymized || o.GuestName != nil {
		t.Fatalf("owner still sees the erased person: %+v", o)
	}
	// Other visits are untouched.
	if o := getVisit(t, admin.AccessToken, other.ID); o.Anonymized || o.GuestName == nil {
		t.Fatalf("another visit was affected: %+v", o)
	}

	// Idempotent, and audited exactly once.
	assertStatus(t, do(t, "DELETE", url, admin.AccessToken, nil), http.StatusOK, "erase again")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visit_erased' AND target = $1 AND user_id = $2`, "visit:"+v.ID, admin.User.ID); n != 1 {
		t.Fatalf("visit_erased audit rows = %d, want 1", n)
	}
}
