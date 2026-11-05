package e2e

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const periodQuery = "from=2026-09-01&to=2026-09-30"

// at is a moment in September 2026, in WIB (the cluster's time), as the SQL string the owner pool takes.
func at(day, hour, min int) string {
	return time.Date(2026, 9, day, hour, min, 0, 0, wib).Format("2006-01-02 15:04:05-07:00")
}

func setTimes(t *testing.T, id, checkin, checkout string) {
	t.Helper()
	ctx := context.Background()
	if checkout == "" {
		if _, err := owner.Exec(ctx, `UPDATE visits SET checkin_at = $2::timestamptz WHERE id = $1`, id, checkin); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := owner.Exec(ctx, `UPDATE visits SET checkin_at = $2::timestamptz, checkout_at = $3::timestamptz WHERE id = $1`, id, checkin, checkout); err != nil {
		t.Fatal(err)
	}
}

// parseCSV strips the byte-order mark and returns the header and the rows.
func parseCSV(t *testing.T, rec *httptest.ResponseRecorder) (header []string, rows [][]string) {
	t.Helper()
	body := rec.Body.String()
	if !strings.HasPrefix(body, "\xEF\xBB\xBF") {
		t.Fatal("the CSV does not start with a UTF-8 byte-order mark")
	}
	all, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("the body is not valid CSV: %v\n%.300s", err, body)
	}
	if len(all) == 0 {
		t.Fatal("empty CSV")
	}
	return all[0], all[1:]
}

func rowAt(t *testing.T, rows [][]string, time string) []string {
	t.Helper()
	for _, r := range rows {
		if r[0] == time {
			return r
		}
	}
	t.Fatalf("no row at %s in %d rows", time, len(rows))
	return nil
}

type reportFixture struct {
	block        string // the houses' block, which a report prints as "Blok <block> No <n>"
	house, other string
	admin, guard tokenPair
	owner        tokenPair
	v1, v2       string
}

func newReportFixture(t *testing.T, tag string) reportFixture {
	t.Helper()
	// Houses and accounts are named after the tag, so every test gets its own (names must be unique).
	f := reportFixture{block: "R" + tag, house: newHouse(t, "R"+tag, "1", true), other: newHouse(t, "R"+tag, "2", true)}
	em := strings.ToLower(tag)
	seed(t, "Admin "+tag, "admin-rp-"+em+"@example.test", "admin", nil)
	seed(t, "Guard "+tag, "guard-rp-"+em+"@example.test", "guard", nil)
	seed(t, "Owner "+tag, "owner-rp-"+em+"@example.test", "owner", f.house)
	f.admin, f.guard, f.owner = login(t, "admin-rp-"+em+"@example.test"), login(t, "guard-rp-"+em+"@example.test"), login(t, "owner-rp-"+em+"@example.test")

	in := func(house, name, category, plate string) string {
		body := map[string]any{"house_id": house, "category": category}
		if name != "" {
			body["guest_name"] = name
		}
		if plate != "" {
			body["vehicle_plate"] = plate
		}
		// Plates need a photo outside the fast lane, so go through the form.
		fields := map[string]string{"house_id": house, "category": category}
		if name != "" {
			fields["guest_name"] = name
		}
		files := map[string][]byte{}
		if plate != "" {
			fields["vehicle_plate"] = plate
			files["vehicle_photo"] = testJPEG(t, 60, 40, "")
		}
		rec := doForm(t, "/v1/visits", f.guard.AccessToken, fields, files)
		assertStatus(t, rec, http.StatusCreated, "check-in "+name+category)
		return decode[visitBody](t, rec).ID
	}
	// v1: exited after 75 minutes, rejected by the owner.
	f.v1 = in(f.house, "Zq Budi Report", "tamu", "B 11 ZQR")
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.v1+"/decision", f.owner.AccessToken, map[string]any{"decision": "rejected"}), http.StatusOK, "reject")
	assertStatus(t, do(t, "POST", "/v1/visits/"+f.v1+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout v1")
	setTimes(t, f.v1, at(10, 8, 30), at(10, 9, 45))
	// v2: a courier who never left, so inside and over the limit.
	f.v2 = in(f.house, "", "kurir", "")
	setTimes(t, f.v2, at(11, 14, 0), "")
	// Four guests whose names would run as formulas in a spreadsheet.
	for i, name := range []string{"=1+1", "+1+1", "-1+1", "@SUM(A1)"} {
		id := in(f.house, name, "tamu", "")
		assertStatus(t, do(t, "POST", "/v1/visits/"+id+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout")
		setTimes(t, id, at(12, 9, i), at(12, 10, i))
	}
	// An anonymized record keeps its times but no personal data.
	anon := in(f.house, "Zq Dihapus", "tamu", "B 12 ZQA")
	assertStatus(t, do(t, "POST", "/v1/visits/"+anon+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout")
	setTimes(t, anon, at(14, 9, 0), at(14, 9, 30))
	if _, err := owner.Exec(context.Background(),
		`UPDATE visits SET guest_name = NULL, vehicle_plate = NULL, purpose_note = NULL, anonymized_at = now() WHERE id = $1`, anon); err != nil {
		t.Fatal(err)
	}
	// Names with characters a PDF core font cannot show.
	uni := in(f.house, "Zq Café 日本語", "tamu", "")
	assertStatus(t, do(t, "POST", "/v1/visits/"+uni+"/checkout", f.guard.AccessToken, nil), http.StatusOK, "checkout")
	setTimes(t, uni, at(16, 9, 0), at(16, 10, 0))
	// Period boundaries (WIB): the first and last second of September are in, a second either side is out.
	for _, c := range []struct {
		name, when string
	}{
		{"Zq Awal Bulan", "2026-09-01 00:00:00+07:00"}, {"Zq Akhir Bulan", "2026-09-30 23:59:59+07:00"},
		{"Zq Sebelum", "2026-08-31 23:59:59+07:00"}, {"Zq Sesudah", "2026-10-01 00:00:00+07:00"},
	} {
		setTimes(t, in(f.house, c.name, "tamu", ""), c.when, "")
	}
	// A visit to the other house must never show up when filtering by this one.
	setTimes(t, in(f.other, "Zq Rumah Lain", "tamu", ""), at(10, 8, 30), "")
	return f
}

func (f reportFixture) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, "GET", path, token, nil)
}

// S8: the visit log for a period, as CSV.
func TestReportCSV(t *testing.T) {
	f := newReportFixture(t, "C")
	base := "/v1/reports/visits.csv?" + periodQuery + "&house_id=" + f.house

	rec := f.get(t, base, f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "csv")
	for k, want := range map[string]string{
		"Content-Type":           "text/csv; charset=utf-8",
		"Content-Disposition":    `attachment; filename="laporan-kunjungan-20260901-20260930.csv"`,
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	header, rows := parseCSV(t, rec)
	wantHeader := []string{"Waktu masuk (WIB)", "Waktu keluar (WIB)", "Lama (menit)", "Rumah", "Kategori", "Status", "Keputusan pemilik", "Lewat batas"}
	if !slices.Equal(header, wantHeader) {
		t.Fatalf("header = %v, want %v (no personal columns by default)", header, wantHeader)
	}
	// 8 visits in the period for this house: v1, v2, four formulas, anonymized, unicode = 8, plus the
	// first and last second of the month = 10. The two just outside, and the other house, are not here.
	if len(rows) != 10 {
		t.Fatalf("%d rows, want 10", len(rows))
	}
	for _, r := range rows {
		if len(r) != len(wantHeader) {
			t.Fatalf("row %v has %d cells, want %d", r, len(r), len(wantHeader))
		}
	}
	for _, out := range []string{"2026-08-31 23:59", "2026-10-01 00:00"} {
		for _, r := range rows {
			if r[0] == out {
				t.Fatalf("a visit outside the period (%s) is in the report", out)
			}
		}
	}
	if !slices.IsSortedFunc(rows, func(a, b []string) int { return strings.Compare(b[0], a[0]) }) {
		t.Fatal("rows are not newest first")
	}

	v1 := rowAt(t, rows, "2026-09-10 08:30")
	if want := []string{"2026-09-10 08:30", "2026-09-10 09:45", "75", "Blok " + f.block + " No 1", "tamu", "keluar", "ditolak", "tidak"}; !slices.Equal(v1, want) {
		t.Fatalf("v1 = %v, want %v", v1, want)
	}
	v2 := rowAt(t, rows, "2026-09-11 14:00")
	if want := []string{"2026-09-11 14:00", "", "", "Blok " + f.block + " No 1", "kurir", "di dalam", "menunggu", "ya"}; !slices.Equal(v2, want) {
		t.Fatalf("v2 = %v, want %v (still inside, long past the limit)", v2, want)
	}
	// No personal data anywhere in the default report.
	for _, secret := range []string{"Zq Budi", "ZQR", "Zq Dihapus", "Caf", "1+1", "SUM"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("the default report contains %q", secret)
		}
	}

	// With personal data: names, plates and notes appear; an anonymized record stays blank.
	rec = f.get(t, base+"&include_personal=true", f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "csv with personal data")
	header, rows = parseCSV(t, rec)
	if len(header) != 11 || header[8] != "Nama tamu" || header[9] != "Plat nomor" || header[10] != "Keterangan" {
		t.Fatalf("header = %v, want the three personal columns added", header)
	}
	if got := rowAt(t, rows, "2026-09-10 08:30"); got[8] != "Zq Budi Report" || got[9] != "B 11 ZQR" || got[10] != "" {
		t.Fatalf("v1 personal cells = %v", got[8:])
	}
	if got := rowAt(t, rows, "2026-09-14 09:00"); got[8] != "" || got[9] != "" || got[10] != "" {
		t.Fatalf("an anonymized record shows personal data: %v", got[8:])
	}
	if got := rowAt(t, rows, "2026-09-16 09:00"); got[8] != "Zq Café 日本語" {
		t.Fatalf("a UTF-8 name came out as %q", got[8])
	}

	// CSV injection: names that start like a formula get a leading apostrophe.
	for i, want := range []string{"'=1+1", "'+1+1", "'-1+1", "'@SUM(A1)"} {
		if got := rowAt(t, rows, fmt.Sprintf("2026-09-12 09:0%d", i)); got[8] != want {
			t.Fatalf("formula name %d came out as %q, want %q", i, got[8], want)
		}
	}
	for _, r := range rows {
		for _, cell := range r {
			if cell != "" && strings.ContainsAny(cell[:1], "=+-@\t\r") {
				t.Fatalf("a cell starts with a formula character and would run in a spreadsheet: %q", cell)
			}
		}
	}

	// Filters.
	_, only := parseCSV(t, f.get(t, base+"&category=kurir", f.admin.AccessToken))
	if len(only) != 1 || only[0][4] != "kurir" {
		t.Fatalf("category=kurir gave %v", only)
	}
	_, inside := parseCSV(t, f.get(t, base+"&status=inside", f.admin.AccessToken))
	for _, r := range inside {
		if r[5] != "di dalam" {
			t.Fatalf("status=inside returned %v", r)
		}
	}
	if len(inside) < 3 {
		t.Fatalf("status=inside returned %d rows, want at least the courier and the two boundary visits", len(inside))
	}
	_, otherRows := parseCSV(t, f.get(t, "/v1/reports/visits.csv?"+periodQuery+"&house_id="+f.other, f.admin.AccessToken))
	if len(otherRows) != 1 || otherRows[0][3] != "Blok "+f.block+" No 2" {
		t.Fatalf("the other house's report = %v", otherRows)
	}
	// A period with no visits still answers with the header, and is still audited.
	empty := f.get(t, "/v1/reports/visits.csv?from=2020-01-01&to=2020-01-31&house_id="+f.house, f.admin.AccessToken)
	assertStatus(t, empty, http.StatusOK, "empty period")
	if _, none := parseCSV(t, empty); len(none) != 0 {
		t.Fatalf("an empty period returned %d rows", len(none))
	}
	// A period that reaches into the future is fine.
	assertStatus(t, f.get(t, "/v1/reports/visits.csv?from="+day(0)+"&to="+day(30)+"&house_id="+f.house, f.admin.AccessToken), http.StatusOK, "future end")

	// Audit: one row per export, written by the admin, with the scope and count and no personal data.
	rowsAudit, err := owner.Query(context.Background(),
		`SELECT target FROM audit_logs WHERE action = 'visits_exported' AND user_id = $1 ORDER BY id`, f.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rowsAudit.Close()
	var targets []string
	for rowsAudit.Next() {
		var s string
		if err := rowsAudit.Scan(&s); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, s)
	}
	if len(targets) < 6 {
		t.Fatalf("%d export audit rows, want one per export (6 so far)", len(targets))
	}
	if want := "format:csv from:2026-09-01 to:2026-09-30 personal:false rows:10 house:" + f.house; targets[0] != want {
		t.Fatalf("first audit target = %q, want %q", targets[0], want)
	}
	if want := "format:csv from:2026-09-01 to:2026-09-30 personal:true rows:10 house:" + f.house; targets[1] != want {
		t.Fatalf("second audit target = %q, want %q", targets[1], want)
	}
	if want := "format:csv from:2020-01-01 to:2020-01-31 personal:false rows:0 house:" + f.house; !slices.Contains(targets, want) {
		t.Fatalf("the empty export was not audited: %v", targets)
	}
	for _, tg := range targets {
		if strings.Contains(strings.ToLower(tg), "zq") && !strings.Contains(tg, f.house) {
			t.Fatalf("an audit target contains personal data: %q", tg)
		}
		if len(tg) > 200 {
			t.Fatalf("audit target is %d characters, over the 200 limit", len(tg))
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported' AND (target ILIKE '%Budi%' OR target ILIKE '%ZQR%' OR target ILIKE '%Dihapus%')`); n != 0 {
		t.Fatalf("%d export audit rows contain a name or plate", n)
	}
}

func TestReportValidationAndAccess(t *testing.T) {
	f := newReportFixture(t, "V")
	for name, q := range map[string]string{
		"missing from":         "to=2026-09-30",
		"missing to":           "from=2026-09-01",
		"nothing":              "",
		"bad date":             "from=besok&to=2026-09-30",
		"inverted":             "from=2026-09-30&to=2026-09-01",
		"94 days":              "from=2026-07-01&to=2026-10-02",
		"bad house":            periodQuery + "&house_id=nope",
		"bad category":         periodQuery + "&category=vip",
		"bad status":           periodQuery + "&status=maybe",
		"bad include_personal": periodQuery + "&include_personal=yes",
	} {
		for _, ext := range []string{"csv", "pdf"} {
			assertStatus(t, f.get(t, "/v1/reports/visits."+ext+"?"+q, f.admin.AccessToken), http.StatusUnprocessableEntity, name+" ("+ext+")")
		}
	}
	// 93 days, inclusive, is the longest allowed.
	assertStatus(t, f.get(t, "/v1/reports/visits.csv?from=2026-07-01&to=2026-10-01&house_id="+f.house, f.admin.AccessToken), http.StatusOK, "93 days")

	// A refused request is not an export and is not audited.
	before := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported'`)
	assertStatus(t, f.get(t, "/v1/reports/visits.csv?from=2026-09-30&to=2026-09-01", f.admin.AccessToken), http.StatusUnprocessableEntity, "inverted again")
	if after := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported'`); after != before {
		t.Fatalf("a refused request was audited as an export (%d -> %d)", before, after)
	}

	// Only admins.
	for _, ext := range []string{"csv", "pdf"} {
		path := "/v1/reports/visits." + ext + "?" + periodQuery
		for name, tok := range map[string]string{"guard": f.guard.AccessToken, "owner": f.owner.AccessToken} {
			assertStatus(t, f.get(t, path, tok), http.StatusForbidden, name+" "+ext)
		}
		assertStatus(t, f.get(t, path, ""), http.StatusUnauthorized, "anonymous "+ext)
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported' AND user_id IN ($1, $2)`, f.guard.User.ID, f.owner.User.ID); n != 0 {
		t.Fatalf("%d exports are attributed to a guard or an owner", n)
	}
}

func TestReportCSVPagesThroughLargePeriods(t *testing.T) {
	f := newReportFixture(t, "P")
	bulk := newHouse(t, "RB", "1", true)
	if _, err := owner.Exec(context.Background(), `
		INSERT INTO visits (house_id, guest_name, category, checkin_by, checkin_at)
		SELECT $1, 'Zq Bulk ' || g, 'tamu', $2, timestamptz '2026-09-15 00:00:00+07' + g * interval '1 minute'
		FROM generate_series(1, 450) g`, bulk, f.guard.User.ID); err != nil {
		t.Fatal(err)
	}
	rec := f.get(t, "/v1/reports/visits.csv?"+periodQuery+"&house_id="+bulk+"&include_personal=true", f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "large csv")
	_, rows := parseCSV(t, rec)
	if len(rows) != 450 {
		t.Fatalf("%d rows, want 450 (200 per page, so three pages)", len(rows))
	}
	seen := map[string]bool{}
	for i, r := range rows {
		if seen[r[8]] {
			t.Fatalf("row %d repeats %q across pages", i, r[8])
		}
		seen[r[8]] = true
		if i > 0 && r[0] >= rows[i-1][0] {
			t.Fatalf("rows %d and %d are not strictly newest first (%s, %s)", i-1, i, rows[i-1][0], r[0])
		}
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported' AND target LIKE '%rows:450 house:'||$1`, bulk); n != 1 {
		t.Fatalf("the large export was audited %d times with its row count, want 1", n)
	}
}

func TestReportPDF(t *testing.T) {
	f := newReportFixture(t, "D")
	base := "/v1/reports/visits.pdf?" + periodQuery + "&house_id=" + f.house

	rec := f.get(t, base, f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "pdf")
	if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="laporan-kunjungan-20260901-20260930.pdf"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "%PDF-") || !strings.Contains(body[len(body)-32:], "%%EOF") {
		t.Fatalf("not a complete PDF: starts %.12q ends %q", body, body[len(body)-16:])
	}
	if rec.Header().Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length %q does not match the %d bytes sent", rec.Header().Get("Content-Length"), len(body))
	}
	for _, want := range []string{"Laporan Kunjungan Buku Tamu", "Periode: 2026-09-01 sampai 2026-09-30", "Total kunjungan: 10", "Masih di dalam: 3", "Halaman 1 dari"} {
		if !strings.Contains(body, want) {
			t.Errorf("the PDF lacks %q", want)
		}
	}
	// Per-category totals and the overstay count come from the same rows as the table.
	for _, want := range []string{"kurir: 1", "tamu: 9", "Lewat batas lama tinggal: 3"} {
		if !strings.Contains(body, want) {
			t.Errorf("the PDF summary lacks %q", want)
		}
	}
	for _, secret := range []string{"Zq Budi", "ZQR", "Zq Dihapus", "1+1", "memuat data pribadi"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the default PDF contains %q", secret)
		}
	}

	// With personal data the guest names appear, with a warning; characters outside Latin-1 do not break it.
	rec = f.get(t, base+"&include_personal=true", f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "pdf with personal data")
	body = rec.Body.String()
	for _, want := range []string{"Zq Budi Report", "B 11 ZQR", "memuat data pribadi", "Zq Caf"} {
		if !strings.Contains(body, want) {
			t.Errorf("the personal PDF lacks %q", want)
		}
	}
	if !strings.HasPrefix(body, "%PDF-") {
		t.Fatal("the personal PDF is not a PDF")
	}

	// A filter is stated on the page, and an empty period still produces a PDF that says so.
	rec = f.get(t, base+"&category=kurir", f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "pdf for one category")
	if !strings.Contains(rec.Body.String(), "Filter - ") || !strings.Contains(rec.Body.String(), "kategori: kurir") || !strings.Contains(rec.Body.String(), "Total kunjungan: 1") {
		t.Error("the filtered PDF does not state its filter and total")
	}
	rec = f.get(t, "/v1/reports/visits.pdf?from=2020-01-01&to=2020-01-31&house_id="+f.house, f.admin.AccessToken)
	assertStatus(t, rec, http.StatusOK, "empty pdf")
	if !strings.Contains(rec.Body.String(), "Tidak ada kunjungan pada periode ini.") {
		t.Error("an empty PDF does not say there were no visits")
	}
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported' AND target LIKE 'format:pdf %' AND user_id = $1`, f.admin.User.ID); n != 4 {
		t.Fatalf("%d PDF exports audited, want 4", n)
	}
}

func TestReportPDFRefusesWhatCSVCanStream(t *testing.T) {
	f := newReportFixture(t, "X")
	huge := newHouse(t, "RH", "1", true)
	if _, err := owner.Exec(context.Background(), `
		INSERT INTO visits (house_id, guest_name, category, checkin_by, checkin_at)
		SELECT $1, 'Zq Banyak', 'tamu', $2, timestamptz '2026-09-01 00:00:00+07' + g * interval '1 minute'
		FROM generate_series(1, 5001) g`, huge, f.guard.User.ID); err != nil {
		t.Fatal(err)
	}
	before := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported'`)
	rec := f.get(t, "/v1/reports/visits.pdf?"+periodQuery+"&house_id="+huge, f.admin.AccessToken)
	assertStatus(t, rec, http.StatusUnprocessableEntity, "pdf over the row cap")
	if !strings.Contains(rec.Body.String(), "CSV") || !strings.Contains(rec.Body.String(), "5001") {
		t.Fatalf("the refusal should say how many rows and suggest the CSV: %s", rec.Body)
	}
	if after := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'visits_exported'`); after != before {
		t.Fatal("a refused PDF was audited as an export")
	}
	// The same data is fine as a CSV.
	csvRec := f.get(t, "/v1/reports/visits.csv?"+periodQuery+"&house_id="+huge, f.admin.AccessToken)
	assertStatus(t, csvRec, http.StatusOK, "csv with the same rows")
	if _, rows := parseCSV(t, csvRec); len(rows) != 5001 {
		t.Fatalf("%d CSV rows, want 5001", len(rows))
	}
}
