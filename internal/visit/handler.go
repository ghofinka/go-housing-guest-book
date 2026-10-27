package visit

import (
	"errors"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/photo"
)

const (
	maxNameLen   = 100
	maxNoteLen   = 200
	maxPlateLen  = 12
	defaultLimit = 50
	maxLimit     = 200
)

var validCategories = map[string]bool{
	"tamu": true, "kurir": true, "ojol": true, "tukang": true, "keluarga": true, "lainnya": true,
}

// maxRequestBytes caps a multipart check-in: two photos plus form fields.
const maxRequestBytes = 2*photo.MaxUploadBytes + 1<<20

// Handler exposes /v1/visits. Only guards check guests in and out. Guards and
// admins read everything; owners read only their own house's visits (enforced by
// the service and again by row-level security).
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	guard := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, "guard") }
	reader := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, "guard", "admin", "owner") }
	mux.Handle("POST /v1/visits", guard(h.checkIn))
	mux.Handle("POST /v1/visits/{id}/checkout", guard(h.checkOut))
	mux.Handle("DELETE /v1/visits/{id}/personal-data", h.authn.Require(http.HandlerFunc(h.erase), auth.RoleAdmin))
	mux.Handle("GET /v1/visits", reader(h.list))
	mux.Handle("GET /v1/visits/{id}", reader(h.get))
	mux.Handle("GET /v1/visits/{id}/photos/{kind}", reader(h.photo))
}

type checkInRequest struct {
	ClientRequestID *uuid.UUID `json:"client_request_id"`
	HouseID         *uuid.UUID `json:"house_id"`
	GuestName       *string    `json:"guest_name"`
	Category        string     `json:"category"`
	PurposeNote     *string    `json:"purpose_note"`
	VehiclePlate    *string    `json:"vehicle_plate"`
}

// checkIn accepts JSON (no photos) or multipart/form-data. The multipart form has
// the same fields as the JSON body plus the files "vehicle_photo" and "face_photo".
// A vehicle photo is required whenever a plate is given, so a guard cannot log a
// vehicle without the visual evidence the PRD asks for.
func (h *Handler) checkIn(w http.ResponseWriter, r *http.Request) {
	var req checkInRequest
	var vehicleFile, faceFile *multipart.FileHeader
	fields := map[string]string{}

	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "multipart/form-data" {
		if !readCheckInForm(w, r, &req, fields, &vehicleFile, &faceFile) {
			return
		}
		defer r.MultipartForm.RemoveAll() // temp files for large uploads
	} else if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	in := CheckInInput{
		ClientRequestID: req.ClientRequestID,
		Category:        req.Category,
		GuestName:       blankToNil(req.GuestName),
		PurposeNote:     blankToNil(req.PurposeNote),
		VehiclePlate:    blankToNil(req.VehiclePlate),
	}
	if req.HouseID == nil || *req.HouseID == uuid.Nil {
		if _, bad := fields["house_id"]; !bad {
			fields["house_id"] = "Nomor rumah tujuan wajib dipilih."
		}
	} else {
		in.HouseID = *req.HouseID
	}
	if !validCategories[in.Category] {
		fields["category"] = "Kategori harus tamu, kurir, ojol, tukang, keluarga, atau lainnya."
	}
	switch {
	case in.GuestName == nil && !FastLane(in.Category):
		fields["guest_name"] = "Nama tamu wajib diisi."
	case in.GuestName != nil && utf8.RuneCountInString(*in.GuestName) > maxNameLen:
		fields["guest_name"] = "Nama maksimal " + strconv.Itoa(maxNameLen) + " karakter."
	}
	if in.PurposeNote != nil && utf8.RuneCountInString(*in.PurposeNote) > maxNoteLen {
		fields["purpose_note"] = "Keterangan maksimal " + strconv.Itoa(maxNoteLen) + " karakter."
	}
	if in.VehiclePlate != nil {
		plate, ok := normalizePlate(*in.VehiclePlate)
		if !ok {
			fields["vehicle_plate"] = "Plat nomor hanya huruf dan angka, maksimal " + strconv.Itoa(maxPlateLen) + " karakter."
		}
		in.VehiclePlate = &plate
	}
	if in.VehiclePlate != nil && vehicleFile == nil {
		fields["vehicle_photo"] = "Foto kendaraan wajib jika tamu membawa kendaraan."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}

	// Decode and compress only once the cheap checks have passed.
	var err error
	if in.VehiclePhoto, err = processFile(vehicleFile); err != nil {
		photoProblem(w, r, "vehicle_photo", err)
		return
	}
	if in.FacePhoto, err = processFile(faceFile); err != nil {
		photoProblem(w, r, "face_photo", err)
		return
	}

	p, _ := auth.PrincipalFrom(r.Context())
	v, created, err := h.svc.CheckIn(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // idempotent retry of an earlier check-in
	}
	httpx.JSON(w, status, v)
}

func (h *Handler) checkOut(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v, err := h.svc.CheckOut(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// erase handles a data-subject deletion request: photos and personal fields are
// removed now, the record stays (without them) for statistics. Admin only.
func (h *Handler) erase(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v, err := h.svc.Erase(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	v, err := h.svc.Get(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, v)
}

// photo streams a stored photo. It is only reachable with a valid Bearer token and
// goes through the same row-level-security check as the visit itself.
func (h *Handler) photo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if kind != KindVehicle && kind != KindFace {
		httpx.Error(w, http.StatusNotFound, "photo_not_found", "Foto tidak ditemukan.")
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	f, err := h.svc.OpenPhoto(r.Context(), p, id, kind)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: defaultLimit}
	fields := map[string]string{}
	if f.Status != "" && f.Status != StatusInside && f.Status != StatusExited {
		fields["status"] = "Status harus inside atau exited."
	}
	if v := q.Get("house_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["house_id"] = "house_id tidak valid."
		}
		f.HouseID = &id
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			fields["limit"] = "limit harus 1 sampai " + strconv.Itoa(maxLimit) + "."
		} else {
			f.Limit = n
		}
	}
	// from/to are calendar days in WIB (YYYY-MM-DD), both inclusive, matched on check-in time.
	if d, ok := httpx.ParseDay(q.Get("from"), "from", fields); ok {
		f.From = &d
	}
	if d, ok := httpx.ParseDay(q.Get("to"), "to", fields); ok {
		end := d.AddDate(0, 0, 1) // exclusive bound = start of the next day
		f.Before = &end
	}
	if f.From != nil && f.Before != nil && !f.From.Before(*f.Before) {
		fields["to"] = "Tanggal akhir tidak boleh sebelum tanggal awal."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := h.svc.List(r.Context(), p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.Error(w, http.StatusNotFound, "visit_not_found", "Data kunjungan tidak ditemukan.")
	case errors.Is(err, ErrPhotoNotFound):
		httpx.Error(w, http.StatusNotFound, "photo_not_found", "Foto tidak ditemukan.")
	case errors.Is(err, ErrFacePhotoRequired):
		httpx.ValidationError(w, map[string]string{"face_photo": "Foto wajah wajib sesuai kebijakan perumahan."})
	case errors.Is(err, ErrHouseInvalid):
		httpx.ValidationError(w, map[string]string{"house_id": "Rumah tidak ditemukan atau tidak aktif."})
	case errors.Is(err, ErrBadCursor):
		httpx.ValidationError(w, map[string]string{"cursor": "Cursor tidak valid."})
	default:
		slog.Error("visit request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "visit_not_found", "Data kunjungan tidak ditemukan.")
		return uuid.Nil, false
	}
	return id, true
}

// readCheckInForm parses a multipart check-in into req. Problems with individual
// values go into fields; it returns false only when it already wrote a response.
func readCheckInForm(w http.ResponseWriter, r *http.Request, req *checkInRequest, fields map[string]string, vehicle, face **multipart.FileHeader) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "request_too_large", "Ukuran foto terlalu besar.")
		} else {
			httpx.Error(w, http.StatusBadRequest, "invalid_form", "Format data tidak valid.")
		}
		return false
	}
	form := r.MultipartForm
	val := func(key string) *string {
		if v := form.Value[key]; len(v) > 0 {
			return &v[0]
		}
		return nil
	}
	parseUUID := func(key string) *uuid.UUID {
		s := blankToNil(val(key))
		if s == nil {
			return nil
		}
		id, err := uuid.Parse(*s)
		if err != nil {
			fields[key] = key + " tidak valid."
			return nil
		}
		return &id
	}
	req.HouseID = parseUUID("house_id")
	req.ClientRequestID = parseUUID("client_request_id")
	req.GuestName, req.PurposeNote, req.VehiclePlate = val("guest_name"), val("purpose_note"), val("vehicle_plate")
	if c := val("category"); c != nil {
		req.Category = *c
	}
	first := func(key string) *multipart.FileHeader {
		if fh := form.File[key]; len(fh) > 0 {
			return fh[0]
		}
		return nil
	}
	*vehicle, *face = first("vehicle_photo"), first("face_photo")
	return true
}

// processFile compresses an uploaded photo; a nil file means "no photo".
func processFile(fh *multipart.FileHeader) ([]byte, error) {
	if fh == nil {
		return nil, nil
	}
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return photo.Process(f)
}

func photoProblem(w http.ResponseWriter, r *http.Request, field string, err error) {
	switch {
	case errors.Is(err, photo.ErrTooLarge):
		httpx.ValidationError(w, map[string]string{field: "Foto maksimal 10 MB."})
	case errors.Is(err, photo.ErrInvalidImage):
		httpx.ValidationError(w, map[string]string{field: "Foto harus berupa gambar JPEG atau PNG yang valid."})
	default:
		slog.Error("process photo failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
	}
}

// blankToNil trims s and maps empty input to nil.
func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// normalizePlate upper-cases a plate and collapses whitespace ("b  1234 xyz" -> "B 1234 XYZ").
// It accepts only letters, digits and single spaces.
func normalizePlate(s string) (string, bool) {
	p := strings.ToUpper(strings.Join(strings.Fields(s), " "))
	if utf8.RuneCountInString(p) > maxPlateLen {
		return p, false
	}
	for _, r := range p {
		if r != ' ' && !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r))) {
			return p, false
		}
	}
	return p, true
}
