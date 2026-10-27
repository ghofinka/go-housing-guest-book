package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type noticeBody struct {
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
	Retention struct {
		PhotoDays        int `json:"photo_days"`
		PersonalDataDays int `json:"personal_data_days"`
	} `json:"retention"`
	Contact  string `json:"contact"`
	Sections []struct {
		Heading string   `json:"heading"`
		Items   []string `json:"items"`
	} `json:"sections"`
}

// text flattens every section into one string, for "does the notice say X" checks.
func (n noticeBody) text() string {
	var b strings.Builder
	for _, s := range n.Sections {
		b.WriteString(s.Heading + "\n" + strings.Join(s.Items, "\n") + "\n")
	}
	return b.String()
}

func (n noticeBody) section(heading string) []string {
	for _, s := range n.Sections {
		if s.Heading == heading {
			return s.Items
		}
	}
	return nil
}

func fetchNotice(t *testing.T) noticeBody {
	t.Helper()
	rec := do(t, "GET", "/v1/privacy-notice", "", nil) // no token on purpose: guests read it
	assertStatus(t, rec, http.StatusOK, "public notice")
	return decode[noticeBody](t, rec)
}

func resetPrivacySettings(t *testing.T) {
	t.Helper()
	reset := func() {
		for k, v := range map[string]string{
			"photo_retention_days": "30", "personal_data_retention_days": "90",
			"face_photo_required": "false", "privacy_contact": "", "privacy_extra_note": "",
		} {
			if _, err := owner.Exec(context.Background(),
				`UPDATE settings SET value = $2, updated_at = now() WHERE key = $1`, k, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	reset()
	t.Cleanup(reset)
}

func TestPrivacyNoticeIsPublicAndLive(t *testing.T) {
	resetPrivacySettings(t)
	seed(t, "Admin PN", "admin-pn@example.test", "admin", nil)
	admin := login(t, "admin-pn@example.test")

	// Public JSON, no login, with the defaults the cluster agreed on.
	rec := do(t, "GET", "/v1/privacy-notice", "", nil)
	assertStatus(t, rec, http.StatusOK, "json")
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "public") || strings.Contains(cc, "no-store") {
		t.Fatalf("notice should be briefly cacheable, Cache-Control = %q", cc)
	}
	n := decode[noticeBody](t, rec)
	if n.Retention.PhotoDays != 30 || n.Retention.PersonalDataDays != 90 {
		t.Fatalf("retention = %+v, want 30/90", n.Retention)
	}
	keep := strings.Join(n.section("Berapa lama disimpan"), " ")
	if !strings.Contains(keep, "30 hari") || !strings.Contains(keep, "90 hari") {
		t.Fatalf("retention section = %q", keep)
	}
	for _, heading := range []string{"Data apa yang dicatat", "Untuk apa", "Siapa yang dapat melihat", "Berapa lama disimpan", "Hak Anda", "Kontak"} {
		if len(n.section(heading)) == 0 {
			t.Errorf("notice has no %q section", heading)
		}
	}
	if !strings.Contains(n.text(), "tidak menahan KTP") {
		t.Error("the notice should say identity cards are not kept")
	}
	// No contact configured: say so plainly rather than invent one.
	if n.Contact != "" || !strings.Contains(strings.Join(n.section("Kontak"), " "), "pengurus perumahan") {
		t.Fatalf("with no contact set: contact=%q section=%v", n.Contact, n.section("Kontak"))
	}
	if n.section("Catatan dari pengurus") != nil {
		t.Error("no extra note was set but the section is present")
	}

	// Public HTML page for the poster / QR code.
	page := do(t, "GET", "/privacy", "", nil)
	assertStatus(t, page, http.StatusOK, "html")
	if ct := page.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	csp := page.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("missing strict CSP: %q", csp)
	}
	html := page.Body.String()
	for _, want := range []string{`<html lang="id">`, "Pemberitahuan Privasi", "30 hari", "90 hari", "Berapa lama disimpan"} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(html), "<script") || strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Error("the page must be self-contained: no scripts or external resources")
	}

	// It follows the settings: change retention and both views change at once.
	assertStatus(t, do(t, "PUT", "/v1/settings/retention", admin.AccessToken,
		map[string]int{"photo_retention_days": 14, "personal_data_retention_days": 60}), http.StatusOK, "change retention")
	n = fetchNotice(t)
	if n.Retention.PhotoDays != 14 || !strings.Contains(strings.Join(n.section("Berapa lama disimpan"), " "), "14 hari") {
		t.Fatalf("notice did not follow the retention setting: %+v", n.Retention)
	}
	if h := do(t, "GET", "/privacy", "", nil).Body.String(); !strings.Contains(h, "14 hari") || !strings.Contains(h, "60 hari") {
		t.Error("HTML page did not follow the retention setting")
	}

	// ...and the face-photo policy.
	face := func() string { return strings.Join(fetchNotice(t).section("Data apa yang dicatat"), " ") }
	if got := face(); !strings.Contains(got, "hanya diambil jika Anda setuju") {
		t.Fatalf("optional policy wording missing: %q", got)
	}
	if _, err := owner.Exec(context.Background(), `UPDATE settings SET value = 'true' WHERE key = 'face_photo_required'`); err != nil {
		t.Fatal(err)
	}
	if got := face(); !strings.Contains(got, "wajib sesuai kebijakan") {
		t.Fatalf("required policy wording missing: %q", got)
	}
}

func TestPrivacyNoticeAdminSettings(t *testing.T) {
	resetPrivacySettings(t)
	seed(t, "Admin PS", "admin-ps@example.test", "admin", nil)
	seed(t, "Guard PS", "guard-ps@example.test", "guard", nil)
	houseID := newHouse(t, "PS", "1", true)
	seed(t, "Owner PS", "owner-ps@example.test", "owner", houseID)
	admin, guard, own := login(t, "admin-ps@example.test"), login(t, "guard-ps@example.test"), login(t, "owner-ps@example.test")
	before := fetchNotice(t).UpdatedAt

	// Only admins can read or change the editable parts.
	assertStatus(t, do(t, "GET", "/v1/settings/privacy", "", nil), http.StatusUnauthorized, "anonymous get")
	for name, tok := range map[string]string{"guard": guard.AccessToken, "owner": own.AccessToken} {
		assertStatus(t, do(t, "GET", "/v1/settings/privacy", tok, nil), http.StatusForbidden, name+" get")
		assertStatus(t, do(t, "PUT", "/v1/settings/privacy", tok, map[string]string{"contact": "x"}), http.StatusForbidden, name+" put")
	}
	for name, body := range map[string]map[string]any{
		"empty body":       {},
		"contact too long": {"contact": strings.Repeat("a", 201)},
		"note too long":    {"extra_note": strings.Repeat("a", 1001)},
		"unknown field":    {"contact": "x", "legal_basis": "y"},
	} {
		want := http.StatusUnprocessableEntity
		if name == "unknown field" {
			want = http.StatusBadRequest
		}
		if code := do(t, "PUT", "/v1/settings/privacy", admin.AccessToken, body).Code; code != want {
			t.Errorf("%s: status = %d, want %d", name, code, want)
		}
	}

	// Set the contact and a note: they show up in both public views.
	rec := do(t, "PUT", "/v1/settings/privacy", admin.AccessToken,
		map[string]string{"contact": "  Pak RT 08, 0812-0000-0000  ", "extra_note": "CCTV hanya di pos jaga."})
	assertStatus(t, rec, http.StatusOK, "set contact and note")
	got := decode[struct {
		Contact   string `json:"contact"`
		ExtraNote string `json:"extra_note"`
	}](t, rec)
	if got.Contact != "Pak RT 08, 0812-0000-0000" || got.ExtraNote != "CCTV hanya di pos jaga." {
		t.Fatalf("saved = %+v (contact should be trimmed)", got)
	}
	n := fetchNotice(t)
	if n.Contact != "Pak RT 08, 0812-0000-0000" || !strings.Contains(strings.Join(n.section("Kontak"), " "), "Pak RT 08") {
		t.Fatalf("contact not in the notice: %q / %v", n.Contact, n.section("Kontak"))
	}
	if note := n.section("Catatan dari pengurus"); len(note) != 1 || note[0] != "CCTV hanya di pos jaga." {
		t.Fatalf("note section = %v", note)
	}
	if n.UpdatedAt == before {
		t.Error("updated_at did not move after an edit")
	}
	if h := do(t, "GET", "/privacy", "", nil).Body.String(); !strings.Contains(h, "Pak RT 08") || !strings.Contains(h, "CCTV hanya di pos jaga.") {
		t.Error("HTML page is missing the contact or note")
	}

	// Partial update keeps the other field.
	assertStatus(t, do(t, "PUT", "/v1/settings/privacy", admin.AccessToken, map[string]string{"extra_note": ""}), http.StatusOK, "clear note")
	n = fetchNotice(t)
	if n.Contact == "" || n.section("Catatan dari pengurus") != nil {
		t.Fatalf("partial update: contact=%q note=%v", n.Contact, n.section("Catatan dari pengurus"))
	}

	// Admin-typed text can never inject markup into the public page.
	attack := `<script>alert('x')</script><img src=x onerror=alert(1)>`
	assertStatus(t, do(t, "PUT", "/v1/settings/privacy", admin.AccessToken, map[string]string{"contact": attack}), http.StatusOK, "hostile contact")
	html := do(t, "GET", "/privacy", "", nil).Body.String()
	if strings.Contains(html, "<script>") || strings.Contains(html, "<img") {
		t.Fatalf("markup was not escaped:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("the text should appear, escaped")
	}

	// Edits are audited without storing the text itself.
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'privacy_notice_changed' AND user_id = $1 AND target = 'settings:privacy'`, admin.User.ID); n != 3 {
		t.Fatalf("privacy_notice_changed audit rows = %d, want 3", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE target LIKE '%Pak RT%' OR target LIKE '%script%'`); n != 0 {
		t.Fatal("notice text leaked into the audit log")
	}
}

// The public pages must not have been given any other way in: wrong methods are refused.
func TestPrivacyNoticeIsReadOnlyForThePublic(t *testing.T) {
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, path := range []string{"/v1/privacy-notice", "/privacy"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(m, path, strings.NewReader(`{"contact":"x"}`))
			req.RemoteAddr = "192.0.2.50:1"
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404 or 405", m, path, rec.Code)
			}
		}
	}
}
