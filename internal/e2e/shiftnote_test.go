package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// PRD C5: shift handover notes.

type shiftNote struct {
	ID     string `json:"id"`
	Body   string `json:"body"`
	Author struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"author"`
	CreatedAt string `json:"created_at"`
}

type shiftNoteList struct {
	Data       []shiftNote `json:"data"`
	NextCursor *string     `json:"next_cursor"`
}

// clearShiftNotes empties the table, so a test sees only its own notes.
func clearShiftNotes(t *testing.T) {
	t.Helper()
	if _, err := owner.Exec(context.Background(), `DELETE FROM shift_notes`); err != nil {
		t.Fatal(err)
	}
}

func addNote(t *testing.T, token, body string) shiftNote {
	t.Helper()
	rec := do(t, "POST", "/v1/shift-notes", token, map[string]string{"body": body})
	assertStatus(t, rec, http.StatusCreated, "create note")
	return decode[shiftNote](t, rec)
}

// ageNote moves a note's creation time back by days.
func ageNote(t *testing.T, id string, days int) {
	t.Helper()
	if _, err := owner.Exec(context.Background(),
		`UPDATE shift_notes SET created_at = created_at - make_interval(days => $2) WHERE id = $1`, id, days); err != nil {
		t.Fatal(err)
	}
}

func noteBodies(l shiftNoteList) []string {
	out := []string{}
	for _, n := range l.Data {
		out = append(out, n.Body)
	}
	return out
}

func TestShiftNotes(t *testing.T) {
	clearShiftNotes(t)
	t.Cleanup(func() { clearShiftNotes(t) })
	seed(t, "Admin SN", "admin-sn@example.test", "admin", nil)
	seed(t, "Guard SN", "guard-sn@example.test", "guard", nil)
	houseID := newHouse(t, "SN", "1", true)
	seed(t, "Owner SN", "owner-sn@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-sn@example.test"), login(t, "guard-sn@example.test"), login(t, "owner-sn@example.test")

	// Who may do what.
	assertStatus(t, do(t, "GET", "/v1/shift-notes", "", nil), http.StatusUnauthorized, "anonymous list")
	assertStatus(t, do(t, "POST", "/v1/shift-notes", "", map[string]string{"body": "x"}), http.StatusUnauthorized, "anonymous create")
	assertStatus(t, do(t, "GET", "/v1/shift-notes", own.AccessToken, nil), http.StatusForbidden, "owner list")
	assertStatus(t, do(t, "POST", "/v1/shift-notes", own.AccessToken, map[string]string{"body": "x"}), http.StatusForbidden, "owner create")
	assertStatus(t, do(t, "DELETE", "/v1/shift-notes/00000000-0000-0000-0000-000000000001", own.AccessToken, nil), http.StatusForbidden, "owner delete")

	// Length: 1 to 1000 characters after trimming, counted in characters and not bytes.
	for name, body := range map[string]string{"empty": "", "blank": "  \n\t ", "too long": strings.Repeat("a", 1001), "too long multibyte": strings.Repeat("é", 1001)} {
		assertStatus(t, do(t, "POST", "/v1/shift-notes", guard.AccessToken, map[string]string{"body": body}), http.StatusUnprocessableEntity, name)
	}
	if n := count(t, `SELECT count(*) FROM shift_notes`); n != 0 {
		t.Fatalf("a rejected note was stored (%d rows)", n)
	}
	long := strings.Repeat("é", 1000)
	if got := addNote(t, guard.AccessToken, long); got.Body != long {
		t.Fatal("a 1000-character note was changed")
	}
	clearShiftNotes(t)

	// Create: the author is the caller, whatever role.
	first := addNote(t, guard.AccessToken, "  Gerbang 2 rusak, pakai gerbang 1  ")
	if first.Body != "Gerbang 2 rusak, pakai gerbang 1" || first.Author.Name != "Guard SN" || first.Author.ID != guard.User.ID {
		t.Fatalf("created note = %+v", first)
	}
	second := addNote(t, admin.AccessToken, "Tamu ramai sore ini")
	if second.Author.ID != admin.User.ID {
		t.Fatalf("author of the admin's note = %s", second.Author.ID)
	}
	third := addNote(t, guard.AccessToken, "Lampu jalan blok B mati")

	// Both roles read every note, newest first.
	for name, tok := range map[string]string{"guard": guard.AccessToken, "admin": admin.AccessToken} {
		rec := do(t, "GET", "/v1/shift-notes", tok, nil)
		assertStatus(t, rec, http.StatusOK, name+" list")
		got := noteBodies(decode[shiftNoteList](t, rec))
		want := []string{third.Body, second.Body, first.Body}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s sees %v, want %v", name, got, want)
		}
	}

	// Paging.
	page1 := decode[shiftNoteList](t, do(t, "GET", "/v1/shift-notes?limit=2", guard.AccessToken, nil))
	if len(page1.Data) != 2 || page1.NextCursor == nil {
		t.Fatalf("page 1 = %d notes, cursor %v", len(page1.Data), page1.NextCursor)
	}
	page2 := decode[shiftNoteList](t, do(t, "GET", "/v1/shift-notes?limit=2&cursor="+*page1.NextCursor, guard.AccessToken, nil))
	if len(page2.Data) != 1 || page2.NextCursor != nil || page2.Data[0].ID != first.ID {
		t.Fatalf("page 2 = %+v", page2)
	}
	assertStatus(t, do(t, "GET", "/v1/shift-notes?cursor=nonsense", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad cursor")
	assertStatus(t, do(t, "GET", "/v1/shift-notes?limit=0", guard.AccessToken, nil), http.StatusUnprocessableEntity, "limit 0")

	// from/to are days in WIB, both included.
	ageNote(t, first.ID, 10)
	ageNote(t, second.ID, 5)
	day := func(daysAgo int) string {
		var s string
		if err := owner.QueryRow(context.Background(),
			`SELECT to_char((now() AT TIME ZONE 'Asia/Jakarta') - make_interval(days => $1), 'YYYY-MM-DD')`, daysAgo).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	list := func(q string) []string {
		rec := do(t, "GET", "/v1/shift-notes"+q, guard.AccessToken, nil)
		assertStatus(t, rec, http.StatusOK, "list "+q)
		return noteBodies(decode[shiftNoteList](t, rec))
	}
	if got := list("?from=" + day(6)); len(got) != 2 || got[0] != third.Body || got[1] != second.Body {
		t.Fatalf("from 6 days ago = %v, want the two newest", got)
	}
	if got := list("?to=" + day(6)); len(got) != 1 || got[0] != first.Body {
		t.Fatalf("to 6 days ago = %v, want the oldest only", got)
	}
	if got := list("?from=" + day(5) + "&to=" + day(5)); len(got) != 1 || got[0] != second.Body {
		t.Fatalf("a single day = %v, want that day's note", got)
	}
	assertStatus(t, do(t, "GET", "/v1/shift-notes?from=garbage", guard.AccessToken, nil), http.StatusUnprocessableEntity, "bad from")
	assertStatus(t, do(t, "GET", "/v1/shift-notes?from="+day(1)+"&to="+day(3), guard.AccessToken, nil), http.StatusUnprocessableEntity, "to before from")

	// Delete: admin only. A guard cannot erase what the previous shift wrote.
	assertStatus(t, do(t, "DELETE", "/v1/shift-notes/"+third.ID, guard.AccessToken, nil), http.StatusForbidden, "guard delete")
	if n := count(t, `SELECT count(*) FROM shift_notes WHERE id = $1`, third.ID); n != 1 {
		t.Fatal("a guard's delete removed the note")
	}
	assertStatus(t, do(t, "DELETE", "/v1/shift-notes/"+third.ID, admin.AccessToken, nil), http.StatusNoContent, "admin delete")
	assertStatus(t, do(t, "DELETE", "/v1/shift-notes/"+third.ID, admin.AccessToken, nil), http.StatusNotFound, "delete again")
	assertStatus(t, do(t, "DELETE", "/v1/shift-notes/not-a-uuid", admin.AccessToken, nil), http.StatusNotFound, "delete bad id")

	// Audit: by note id only, never the text.
	for action, who := range map[string][2]string{
		"shift_note_created": {guard.User.ID, first.ID},
		"shift_note_deleted": {admin.User.ID, third.ID},
	} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = $1 AND user_id = $2 AND target = $3`, action, who[0], "shift_note:"+who[1]); n != 1 {
			t.Errorf("audit %s rows = %d, want 1", action, n)
		}
	}
	for _, text := range []string{"Gerbang", "Tamu ramai", "Lampu jalan"} {
		if n := count(t, `SELECT count(*) FROM audit_logs WHERE target LIKE '%' || $1 || '%'`, text); n != 0 {
			t.Errorf("note text %q leaked into the audit log", text)
		}
	}
}

func TestShiftNoteRetention(t *testing.T) {
	resetRetentionSettings(t)
	clearShiftNotes(t)
	t.Cleanup(func() { clearShiftNotes(t) })
	seed(t, "Admin SR", "admin-sr@example.test", "admin", nil)
	seed(t, "Guard SR", "guard-sr@example.test", "guard", nil)
	admin, guard := login(t, "admin-sr@example.test"), login(t, "guard-sr@example.test")
	runRetention(t, admin.AccessToken, "") // clear what other tests left behind

	old := addNote(t, guard.AccessToken, "Catatan lama")
	edge := addNote(t, guard.AccessToken, "Catatan hampir kedaluwarsa")
	young := addNote(t, guard.AccessToken, "Catatan baru")
	ageNote(t, old.ID, 31)
	ageNote(t, edge.ID, 29)
	ageNote(t, young.ID, 2)

	// The default limit is 30 days, shown by the settings endpoint.
	st := decode[struct {
		ShiftNoteDays int `json:"shift_note_retention_days"`
	}](t, do(t, "GET", "/v1/settings/retention", admin.AccessToken, nil))
	if st.ShiftNoteDays != 30 {
		t.Fatalf("default shift note limit = %d, want 30", st.ShiftNoteDays)
	}

	// A dry run counts and deletes nothing; a real run deletes only the expired note and says so.
	if dry := runRetention(t, admin.AccessToken, "?dry_run=true"); dry.ShiftNotesDeleted != 1 {
		t.Fatalf("dry run reports %d notes, want 1", dry.ShiftNotesDeleted)
	}
	if n := count(t, `SELECT count(*) FROM shift_notes`); n != 3 {
		t.Fatalf("a dry run changed the notes: %d left", n)
	}
	res := runRetention(t, admin.AccessToken, "")
	if res.ShiftNotesDeleted != 1 {
		t.Fatalf("run deleted %d notes, want 1", res.ShiftNotesDeleted)
	}
	for id, want := range map[string]int{old.ID: 0, edge.ID: 1, young.ID: 1} {
		if n := count(t, `SELECT count(*) FROM shift_notes WHERE id = $1`, id); n != want {
			t.Errorf("note %s rows = %d, want %d", id, n, want)
		}
	}
	if n := count(t, `SELECT shift_notes_deleted FROM retention_runs WHERE id = $1`, res.RunID); n != 1 {
		t.Errorf("retention_runs.shift_notes_deleted = %d, want 1", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'retention_run' AND target LIKE '%shift_notes:1 preregs:%'`); n < 1 {
		t.Error("the run's audit entry does not count the deleted notes")
	}

	// The limit is an admin setting. The older two-field PUT keeps working and leaves it alone.
	assertStatus(t, do(t, "PUT", "/v1/settings/retention", admin.AccessToken,
		map[string]int{"photo_retention_days": 30, "personal_data_retention_days": 90}), http.StatusOK, "two-field put")
	if n := count(t, `SELECT count(*) FROM settings WHERE key = 'shift_note_retention_days' AND value = '30'`); n != 1 {
		t.Fatal("a PUT without the shift note limit changed it")
	}
	for name, v := range map[string]int{"zero": 0, "over a year": 366, "negative": -5} {
		code := do(t, "PUT", "/v1/settings/retention", admin.AccessToken, map[string]int{
			"photo_retention_days": 30, "personal_data_retention_days": 90, "shift_note_retention_days": v}).Code
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", name, code)
		}
	}
	rec := do(t, "PUT", "/v1/settings/retention", admin.AccessToken, map[string]int{
		"photo_retention_days": 30, "personal_data_retention_days": 90, "shift_note_retention_days": 1})
	assertStatus(t, rec, http.StatusOK, "shift note limit 1 day")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'retention_settings_changed' AND user_id = $1 AND target LIKE '%shift_note_retention_days:30->1'`,
		admin.User.ID); n != 1 {
		t.Fatalf("settings audit rows for the shift note limit = %d, want 1", n)
	}
	if res := runRetention(t, admin.AccessToken, ""); res.ShiftNotesDeleted != 2 {
		t.Fatalf("with a 1-day limit the run deleted %d notes, want 2 (the 29- and 2-day-old ones)", res.ShiftNotesDeleted)
	}
}
