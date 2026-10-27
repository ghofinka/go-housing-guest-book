// Package privacy serves the public privacy notice (M8): what the guest book
// records, why, who can see it, how long it is kept, and who to ask.
//
// The text is plain Indonesian, built in code, with the retention numbers and the
// face-photo policy read live from settings so the notice can never disagree with
// what the system actually does. The cluster only has to publish the /privacy URL
// (poster, QR code) before go-live.
package privacy

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/db"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/retention"
)

const (
	settingFaceRequired = "face_photo_required"
	settingContact      = "privacy_contact"
	settingExtraNote    = "privacy_extra_note"

	maxContactLen = 200
	maxNoteLen    = 1000

	// fallbackContact is shown until an admin sets a real contact; the notice must not invent one.
	fallbackContact = "Hubungi pengurus perumahan atau petugas jaga."
)

type Section struct {
	Heading string   `json:"heading"`
	Items   []string `json:"items"`
}

type Notice struct {
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	Retention struct {
		PhotoDays        int `json:"photo_days"`
		PersonalDataDays int `json:"personal_data_days"`
	} `json:"retention"`
	// Contact is what the admin configured; empty means none has been set yet.
	Contact  string    `json:"contact"`
	Sections []Section `json:"sections"`
}

// Editable is the part of the notice an admin can change.
type Editable struct {
	Contact   string `json:"contact"`
	ExtraNote string `json:"extra_note"`
}

type Service struct{ pool *pgxpool.Pool }

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

var allKeys = []string{
	retention.SettingPhotoDays, retention.SettingPersonalDays, settingFaceRequired, settingContact, settingExtraNote,
}

// Notice builds the current notice from settings.
func (s *Service) Notice(ctx context.Context) (Notice, error) {
	rows, err := db.New(s.pool).GetSettings(ctx, allKeys)
	if err != nil {
		return Notice{}, fmt.Errorf("read privacy settings: %w", err)
	}
	days, err := retention.SettingsFromRows(rows)
	if err != nil {
		return Notice{}, err
	}
	vals := map[string]string{}
	var updated time.Time
	for _, r := range rows {
		vals[r.Key] = r.Value
		if r.UpdatedAt.After(updated) {
			updated = r.UpdatedAt
		}
	}
	return build(days, vals[settingFaceRequired] == "true", strings.TrimSpace(vals[settingContact]),
		strings.TrimSpace(vals[settingExtraNote]), updated.In(httpx.WIB)), nil
}

func build(days retention.Settings, faceRequired bool, contact, note string, updated time.Time) Notice {
	n := Notice{Title: "Pemberitahuan Privasi Buku Tamu Digital", UpdatedAt: updated, Contact: contact}
	n.Retention.PhotoDays, n.Retention.PersonalDataDays = days.PhotoDays, days.PersonalDataDays

	face := "Foto wajah: hanya diambil jika Anda setuju atau sesuai kebijakan pengurus perumahan."
	if faceRequired {
		face = "Foto wajah: wajib sesuai kebijakan pengurus perumahan."
	}
	contactLine := fallbackContact
	if contact != "" {
		contactLine = contact
	}

	n.Sections = []Section{
		{"Data apa yang dicatat", []string{
			"Nama Anda (tidak diminta untuk kurir dan ojol).",
			"Rumah yang Anda tuju, kategori, dan keterangan kunjungan.",
			"Plat nomor dan foto kendaraan, jika Anda membawa kendaraan.",
			face,
			"Waktu masuk dan waktu keluar, serta nama petugas jaga yang mencatat.",
			"Kami tidak menahan KTP, SIM, atau identitas fisik Anda.",
		}},
		{"Untuk apa", []string{
			"Menjaga keamanan perumahan dengan mencatat siapa yang masuk, ke rumah mana, dan kapan.",
			"Data tidak dipakai untuk tujuan lain.",
		}},
		{"Siapa yang dapat melihat", []string{
			"Petugas jaga dan pengurus yang berwenang.",
			"Pemilik rumah hanya dapat melihat tamu yang datang ke rumahnya sendiri.",
			"Foto tidak dapat dibuka oleh umum, dan setiap pembukaan data tercatat.",
		}},
		{"Berapa lama disimpan", []string{
			fmt.Sprintf("Foto dihapus otomatis setelah %d hari.", days.PhotoDays),
			fmt.Sprintf("Nama, plat nomor, dan keterangan dihapus setelah %d hari.", days.PersonalDataDays),
			"Waktu dan jumlah kunjungan tanpa identitas dapat disimpan untuk statistik.",
		}},
		{"Hak Anda", []string{
			"Anda dapat meminta data kunjungan Anda dihapus lebih awal kepada pengurus.",
			"Pengelolaan data ini mengikuti prinsip Undang-Undang Nomor 27 Tahun 2022 tentang Pelindungan Data Pribadi.",
		}},
		{"Kontak", []string{contactLine}},
	}
	if note != "" {
		n.Sections = append(n.Sections, Section{"Catatan dari pengurus", []string{note}})
	}
	return n
}

// Editable returns the admin-editable fields.
func (s *Service) Editable(ctx context.Context) (Editable, error) {
	rows, err := db.New(s.pool).GetSettings(ctx, []string{settingContact, settingExtraNote})
	if err != nil {
		return Editable{}, fmt.Errorf("read privacy settings: %w", err)
	}
	var e Editable
	for _, r := range rows {
		switch r.Key {
		case settingContact:
			e.Contact = r.Value
		case settingExtraNote:
			e.ExtraNote = r.Value
		}
	}
	return e, nil
}

// Update stores the given fields (nil = unchanged) and audits the change. The audit
// target names the setting group only, never the text.
func (s *Service) Update(ctx context.Context, actor auth.Principal, contact, note *string) (Editable, error) {
	err := db.InTx(ctx, s.pool, actor.Role, actor.HouseID, func(q *db.Queries) error {
		for key, v := range map[string]*string{settingContact: contact, settingExtraNote: note} {
			if v == nil {
				continue
			}
			if err := q.UpsertSetting(ctx, db.UpsertSettingParams{Key: key, Value: *v}); err != nil {
				return fmt.Errorf("save %s: %w", key, err)
			}
		}
		return q.InsertAuditLog(ctx, db.InsertAuditLogParams{
			UserID: pgtype.UUID{Bytes: actor.UserID, Valid: true}, Action: "privacy_notice_changed", Target: "settings:privacy",
		})
	})
	if err != nil {
		return Editable{}, fmt.Errorf("update privacy notice: %w", err)
	}
	return s.Editable(ctx)
}

var page = template.Must(template.New("notice").Parse(`<!doctype html>
<html lang="id">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font-family:system-ui,sans-serif;line-height:1.5;margin:0;padding:1rem;background:#f6f7f9;color:#1b1f24}
main{max-width:42rem;margin:0 auto;background:#fff;padding:1.25rem 1.5rem;border-radius:.5rem}
h1{font-size:1.4rem;margin-top:0}h2{font-size:1.1rem;margin-bottom:.25rem}
.meta{color:#5a6570;font-size:.9rem}li{margin:.25rem 0;white-space:pre-line}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p class="meta">Terakhir diperbarui: {{.Updated}}</p>
{{range .Sections}}<section>
<h2>{{.Heading}}</h2>
<ul>
{{range .Items}}<li>{{.}}</li>
{{end}}</ul>
</section>
{{end}}</main>
</body>
</html>
`))

// Handler serves the public notice and the admin settings for it.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	// Public on purpose: guests at the gate read it without any account.
	mux.HandleFunc("GET /v1/privacy-notice", h.json)
	mux.HandleFunc("GET /privacy", h.html)
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("GET /v1/settings/privacy", admin(h.getSettings))
	mux.Handle("PUT /v1/settings/privacy", admin(h.putSettings))
}

// publicHeaders overrides the API-wide no-store: the notice holds no personal data and may be cached briefly.
func publicHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=300")
}

func (h *Handler) json(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.Notice(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
		return
	}
	publicHeaders(w)
	httpx.JSON(w, http.StatusOK, n)
}

func (h *Handler) html(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.Notice(r.Context())
	if err != nil {
		http.Error(w, "Terjadi kesalahan. Coba lagi.", http.StatusInternalServerError)
		return
	}
	publicHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	_ = page.Execute(w, struct {
		Notice
		Updated string
	}{n, n.UpdatedAt.Format("2006-01-02")})
}

func (h *Handler) getSettings(w http.ResponseWriter, r *http.Request) {
	e, err := h.svc.Editable(r.Context())
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}

func (h *Handler) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Contact   *string `json:"contact"`
		ExtraNote *string `json:"extra_note"`
	}
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	trim := func(s *string) *string {
		if s == nil {
			return nil
		}
		t := strings.TrimSpace(*s)
		return &t
	}
	contact, note := trim(req.Contact), trim(req.ExtraNote)
	if contact == nil && note == nil {
		fields["body"] = "Tidak ada perubahan yang dikirim."
	}
	if contact != nil && utf8.RuneCountInString(*contact) > maxContactLen {
		fields["contact"] = "Kontak maksimal " + strconv.Itoa(maxContactLen) + " karakter."
	}
	if note != nil && utf8.RuneCountInString(*note) > maxNoteLen {
		fields["extra_note"] = "Catatan maksimal " + strconv.Itoa(maxNoteLen) + " karakter."
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	e, err := h.svc.Update(r.Context(), p, contact, note)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
		return
	}
	httpx.JSON(w, http.StatusOK, e)
}
