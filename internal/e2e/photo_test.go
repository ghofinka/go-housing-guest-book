package e2e

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ghofinka/go-housing-guest-book/internal/db"
)

// testJPEG returns a w x h gradient JPEG; a non-empty secret is embedded in an EXIF APP1 segment.
func testJPEG(t *testing.T, w, h int, secret string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), uint8((x + y) % 256), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if secret == "" {
		return b
	}
	payload := append([]byte("Exif\x00\x00"), []byte(secret)...)
	seg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	return append(append(append([]byte{}, b[:2]...), seg...), b[2:]...)
}

// doForm sends a multipart/form-data request with text fields and file parts.
func doForm(t *testing.T, path, bearer string, fields map[string]string, files map[string][]byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		fw, err := mw.CreateFormFile(name, name+".jpg")
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(data)
	}
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "192.0.2.77:1234"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// photoFiles counts stored photo files (ignores directories).
func photoFiles(t *testing.T) int {
	t.Helper()
	n := 0
	err := fs.WalkDir(os.DirFS(photoDir), ".", func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestVehiclePhotoRules(t *testing.T) {
	seed(t, "Guard P1", "guard-p1@example.test", "guard", nil)
	guard := login(t, "guard-p1@example.test")
	houseID := newHouse(t, "P", "1", true)
	form := func(extra map[string]string) map[string]string {
		f := map[string]string{"house_id": houseID, "guest_name": "Foto Test", "category": "tamu"}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	files0 := photoFiles(t)

	// A plate without a photo is refused, whether sent as JSON or as a form.
	rec := do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "X", "category": "tamu", "vehicle_plate": "B 1 A"})
	assertStatus(t, rec, http.StatusUnprocessableEntity, "plate without photo (json)")
	if !strings.Contains(rec.Body.String(), "vehicle_photo") {
		t.Fatalf("error should name vehicle_photo: %s", rec.Body)
	}
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, form(map[string]string{"vehicle_plate": "B 1 A"}), nil),
		http.StatusUnprocessableEntity, "plate without photo (form)")

	// On foot: no plate, no photo needed.
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, form(nil), nil), http.StatusCreated, "on foot")

	// With a photo: stored compressed, EXIF removed, linked to the visit.
	big := testJPEG(t, 3000, 2000, "GPS-SECRET-1234")
	rec = doForm(t, "/v1/visits", guard.AccessToken, form(map[string]string{"vehicle_plate": "b 1234 xyz"}),
		map[string][]byte{"vehicle_photo": big})
	assertStatus(t, rec, http.StatusCreated, "with photo")
	v := decode[struct {
		ID              string  `json:"id"`
		VehiclePhotoURL *string `json:"vehicle_photo_url"`
		FacePhotoURL    *string `json:"face_photo_url"`
	}](t, rec)
	if v.VehiclePhotoURL == nil || *v.VehiclePhotoURL != "/v1/visits/"+v.ID+"/photos/vehicle" || v.FacePhotoURL != nil {
		t.Fatalf("photo urls: %v / %v", v.VehiclePhotoURL, v.FacePhotoURL)
	}
	if got := photoFiles(t) - files0; got != 1 {
		t.Fatalf("stored %d new files, want 1", got)
	}

	rec = do(t, "GET", *v.VehiclePhotoURL, guard.AccessToken, nil)
	assertStatus(t, rec, http.StatusOK, "download")
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("cache-control = %q, photos must not be cached", cc)
	}
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("downloaded photo is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() > 1280 || b.Dy() > 1280 {
		t.Fatalf("photo not shrunk: %dx%d", b.Dx(), b.Dy())
	}
	if rec.Body.Len() >= len(big) {
		t.Fatalf("photo not compressed: %d >= %d", rec.Body.Len(), len(big))
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("GPS-SECRET")) {
		t.Fatal("EXIF metadata was stored")
	}
	// The DB holds only a relative key, never an absolute path.
	var path string
	if err := owner.QueryRow(context.Background(), `SELECT vehicle_photo_path FROM visits WHERE id = $1`, v.ID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, photoDir) || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) || strings.Contains(path, "..") {
		t.Fatalf("stored path %q should be a plain relative key", path)
	}

	// PNG is accepted too (and stored as JPEG).
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 80, 60)))
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, form(map[string]string{"vehicle_plate": "D 99 Z"}),
		map[string][]byte{"vehicle_photo": pngBuf.Bytes()}), http.StatusCreated, "png photo")

	// Things that are not real photos.
	bomb := new(bytes.Buffer)
	png.Encode(bomb, image.NewGray(image.Rect(0, 0, 7000, 7000)))
	bad := map[string][]byte{
		"text file":   []byte("hello, not a photo"),
		"fake jpeg":   append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("x"), 100)...),
		"script":      []byte("<script>alert(1)</script>"),
		"pixel bomb":  bomb.Bytes(),
		"over 10 MB":  append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 10<<20)...),
		"empty photo": {},
	}
	before := photoFiles(t)
	for name, data := range bad {
		rec := doForm(t, "/v1/visits", guard.AccessToken, form(map[string]string{"vehicle_plate": "B 5 X"}),
			map[string][]byte{"vehicle_photo": data})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422 (%.200s)", name, rec.Code, rec.Body)
		}
	}
	if photoFiles(t) != before {
		t.Fatal("rejected uploads left files behind")
	}
}

func TestFacePhotoPolicy(t *testing.T) {
	seed(t, "Guard P2", "guard-p2@example.test", "guard", nil)
	guard := login(t, "guard-p2@example.test")
	houseID := newHouse(t, "P", "2", true)
	setPolicy := func(v string) {
		if _, err := owner.Exec(context.Background(), `UPDATE settings SET value = $1 WHERE key = 'face_photo_required'`, v); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { setPolicy("false") })
	face := testJPEG(t, 100, 100, "")
	tamu := map[string]string{"house_id": houseID, "guest_name": "Wajah Test", "category": "tamu"}
	kurir := map[string]string{"house_id": houseID, "category": "kurir"}

	// Default policy: optional.
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, tamu, nil), http.StatusCreated, "optional by default")

	setPolicy("true")
	files0 := photoFiles(t)
	rec := doForm(t, "/v1/visits", guard.AccessToken, tamu, nil)
	assertStatus(t, rec, http.StatusUnprocessableEntity, "required, missing")
	if !strings.Contains(rec.Body.String(), "face_photo") {
		t.Fatalf("error should name face_photo: %s", rec.Body)
	}
	assertStatus(t, do(t, "POST", "/v1/visits", guard.AccessToken,
		map[string]any{"house_id": houseID, "guest_name": "Json", "category": "tamu"}), http.StatusUnprocessableEntity, "required, json check-in")
	if photoFiles(t) != files0 {
		t.Fatal("a refused check-in must not store files")
	}
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, kurir, nil), http.StatusCreated, "fast lane exempt")

	rec = doForm(t, "/v1/visits", guard.AccessToken, tamu, map[string][]byte{"face_photo": face})
	assertStatus(t, rec, http.StatusCreated, "required, provided")
	v := decode[struct {
		ID           string  `json:"id"`
		FacePhotoURL *string `json:"face_photo_url"`
	}](t, rec)
	if v.FacePhotoURL == nil {
		t.Fatal("face_photo_url missing")
	}
	assertStatus(t, do(t, "GET", *v.FacePhotoURL, guard.AccessToken, nil), http.StatusOK, "download face photo")
	assertStatus(t, do(t, "GET", "/v1/visits/"+v.ID+"/photos/vehicle", guard.AccessToken, nil), http.StatusNotFound, "no vehicle photo on this visit")

	// A face photo is stored when offered even if the policy does not require it.
	setPolicy("false")
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, tamu, map[string][]byte{"face_photo": face}), http.StatusCreated, "optional, provided")
}

func TestPhotoNoOrphansOnFailedOrRepeatedCheckIn(t *testing.T) {
	seed(t, "Guard P3", "guard-p3@example.test", "guard", nil)
	guard := login(t, "guard-p3@example.test")
	houseID := newHouse(t, "P", "3", true)
	inactiveID := newHouse(t, "P", "4", false)
	jpg := testJPEG(t, 200, 150, "")

	files0 := photoFiles(t)
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken,
		map[string]string{"house_id": inactiveID, "guest_name": "X", "category": "tamu", "vehicle_plate": "B 1 A"},
		map[string][]byte{"vehicle_photo": jpg}), http.StatusUnprocessableEntity, "inactive house")
	if photoFiles(t) != files0 {
		t.Fatal("check-in refused for the house left a photo behind")
	}

	key := uuid.NewString()
	fields := map[string]string{"client_request_id": key, "house_id": houseID, "guest_name": "Retry", "category": "tamu", "vehicle_plate": "B 2 A"}
	files := map[string][]byte{"vehicle_photo": jpg, "face_photo": jpg}
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, fields, files), http.StatusCreated, "first")
	afterFirst := photoFiles(t)
	if afterFirst != files0+2 {
		t.Fatalf("files after first = %d, want %d", afterFirst, files0+2)
	}
	assertStatus(t, doForm(t, "/v1/visits", guard.AccessToken, fields, files), http.StatusOK, "retry")
	if photoFiles(t) != afterFirst {
		t.Fatal("an idempotent retry stored duplicate photos")
	}
}

func TestPhotoAccessControl(t *testing.T) {
	seed(t, "Admin P", "admin-p@example.test", "admin", nil)
	seed(t, "Guard P4", "guard-p4@example.test", "guard", nil)
	houseA, houseB := newHouse(t, "P", "A", true), newHouse(t, "P", "B", true)
	seed(t, "Owner PA", "owner-pa@example.test", "owner", houseA)
	seed(t, "Owner PB", "owner-pb@example.test", "owner", houseB)
	admin, guard := login(t, "admin-p@example.test"), login(t, "guard-p4@example.test")
	ownA, ownB := login(t, "owner-pa@example.test"), login(t, "owner-pb@example.test")

	rec := doForm(t, "/v1/visits", guard.AccessToken,
		map[string]string{"house_id": houseA, "guest_name": "Tamu A", "category": "tamu", "vehicle_plate": "B 7 A"},
		map[string][]byte{"vehicle_photo": testJPEG(t, 120, 90, "")})
	assertStatus(t, rec, http.StatusCreated, "check-in")
	url := "/v1/visits/" + decode[visitBody](t, rec).ID + "/photos/vehicle"

	assertStatus(t, do(t, "GET", url, "", nil), http.StatusUnauthorized, "anonymous")
	assertStatus(t, do(t, "GET", url, ownB.AccessToken, nil), http.StatusNotFound, "owner of another house (same answer as a missing photo)")
	assertStatus(t, do(t, "GET", url, ownA.AccessToken, nil), http.StatusOK, "owner of the same house")
	assertStatus(t, do(t, "GET", url, guard.AccessToken, nil), http.StatusOK, "guard")
	assertStatus(t, do(t, "GET", url, admin.AccessToken, nil), http.StatusOK, "admin")

	assertStatus(t, do(t, "GET", "/v1/visits/"+uuid.NewString()+"/photos/vehicle", guard.AccessToken, nil), http.StatusNotFound, "unknown visit")
	assertStatus(t, do(t, "GET", "/v1/visits/not-a-uuid/photos/vehicle", guard.AccessToken, nil), http.StatusNotFound, "malformed id")
	assertStatus(t, do(t, "GET", strings.TrimSuffix(url, "vehicle")+"passport", guard.AccessToken, nil), http.StatusNotFound, "unknown kind")
	assertStatus(t, do(t, "GET", strings.TrimSuffix(url, "vehicle")+"face", guard.AccessToken, nil), http.StatusNotFound, "no face photo")

	// Every successful view is audited with who and which photo; refused ones are not.
	id := strings.TrimSuffix(strings.TrimPrefix(url, "/v1/visits/"), "/photos/vehicle")
	if n := count(t, `SELECT count(*) FROM audit_logs WHERE action = 'photo_viewed' AND target = $1`,
		"visit:"+id+":vehicle"); n != 3 {
		t.Fatalf("photo_viewed audit rows = %d, want 3 (owner A, guard, admin; the refused owner B is not audited)", n)
	}

	// A purged file (retention) is a clean 404, not a 500.
	var path string
	owner.QueryRow(context.Background(), `SELECT vehicle_photo_path FROM visits WHERE id = $1`, id).Scan(&path)
	if err := os.Remove(photoDir + string(os.PathSeparator) + strings.ReplaceAll(path, "/", string(os.PathSeparator))); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, do(t, "GET", url, guard.AccessToken, nil), http.StatusNotFound, "file purged")
}

// The database must hide another house's photo paths, not just the Go code.
func TestPhotoPathsRowLevelSecurity(t *testing.T) {
	seed(t, "Guard P5", "guard-p5@example.test", "guard", nil)
	guard := login(t, "guard-p5@example.test")
	houseA, houseB := newHouse(t, "P", "RA", true), newHouse(t, "P", "RB", true)
	rec := doForm(t, "/v1/visits", guard.AccessToken,
		map[string]string{"house_id": houseA, "guest_name": "RLS", "category": "tamu", "vehicle_plate": "B 8 A"},
		map[string][]byte{"vehicle_photo": testJPEG(t, 50, 50, "")})
	assertStatus(t, rec, http.StatusCreated, "check-in")
	id := uuid.MustParse(decode[visitBody](t, rec).ID)
	a, b := uuid.MustParse(houseA), uuid.MustParse(houseB)

	lookup := func(role string, house *uuid.UUID) error {
		return db.InTx(context.Background(), app, role, house, func(q *db.Queries) error {
			_, err := q.GetVisitPhotoPaths(context.Background(), id)
			return err
		})
	}
	if err := lookup("owner", &a); err != nil {
		t.Errorf("owner of the house cannot see its photo path: %v", err)
	}
	if err := lookup("owner", &b); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("owner of another house: err = %v, want no rows", err)
	}
	if err := lookup("", nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("no role: err = %v, want no rows", err)
	}
}
