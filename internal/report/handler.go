package report

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
	"github.com/ghofinka/go-housing-guest-book/internal/visit"
)

// Handler exposes the visit report as CSV and PDF. Admin only: a report can hold personal data, and
// nobody else needs to take a whole period out of the system.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	admin := func(f http.HandlerFunc) http.Handler { return h.authn.Require(f, auth.RoleAdmin) }
	mux.Handle("GET /v1/reports/visits.csv", admin(h.csv))
	mux.Handle("GET /v1/reports/visits.pdf", admin(h.pdf))
}

// parse reads and validates the query. It returns false when it already answered with a 422.
func parse(w http.ResponseWriter, r *http.Request) (Params, bool) {
	q := r.URL.Query()
	fields := map[string]string{}
	var p Params

	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"from", &p.From}, {"to", &p.To}} {
		if q.Get(f.name) == "" {
			fields[f.name] = "Wajib diisi (YYYY-MM-DD)."
			continue
		}
		if d, ok := httpx.ParseDay(q.Get(f.name), f.name, fields); ok {
			*f.dst = d
		}
	}
	if fields["from"] == "" && fields["to"] == "" {
		switch {
		case p.To.Before(p.From):
			fields["to"] = "Tanggal akhir tidak boleh sebelum tanggal awal."
		case p.To.Sub(p.From) > time.Duration(MaxPeriodDays-1)*24*time.Hour+time.Hour:
			// Both are midnight WIB, so the span is whole days; the extra hour absorbs nothing but rounding.
			fields["to"] = "Periode maksimal " + strconv.Itoa(MaxPeriodDays) + " hari."
		}
	}

	if v := q.Get("house_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["house_id"] = "house_id tidak valid."
		}
		p.HouseID = &id
	}
	if v := q.Get("category"); v != "" {
		if !visit.ValidCategory(v) {
			fields["category"] = "Kategori harus tamu, kurir, ojol, tukang, keluarga, atau lainnya."
		}
		p.Category = v
	}
	if v := q.Get("status"); v != "" {
		if v != visit.StatusInside && v != visit.StatusExited {
			fields["status"] = "Status harus inside atau exited."
		}
		p.Status = v
	}
	switch q.Get("include_personal") {
	case "", "false":
	case "true":
		p.IncludePersonal = true
	default:
		fields["include_personal"] = "include_personal harus true atau false."
	}

	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return Params{}, false
	}
	return p, true
}

func filename(p Params, ext string) string {
	return "laporan-kunjungan-" + p.From.Format("20060102") + "-" + p.To.Format("20060102") + "." + ext
}

func (h *Handler) csv(w http.ResponseWriter, r *http.Request) {
	p, ok := parse(w, r)
	if !ok {
		return
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	n, err := h.svc.Count(r.Context(), actor, p)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// Recorded before any data leaves, so a download that stops halfway is still on record.
	if err := h.svc.Audit(r.Context(), actor, "csv", p, n); err != nil {
		h.serverError(w, r, err)
		return
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/csv; charset=utf-8")
	hd.Set("Content-Disposition", `attachment; filename="`+filename(p, "csv")+`"`)
	out, err := newCSVWriter(w, p.IncludePersonal)
	if err != nil {
		return // the client is gone; nothing was sent that matters
	}
	rc := http.NewResponseController(w)
	err = h.svc.Each(r.Context(), actor, p, func(page []Row) error {
		if err := out.writePage(page); err != nil {
			return err
		}
		_ = rc.Flush() // best effort: an unflushable writer just sends it all at the end
		return nil
	})
	if err != nil {
		// The status line is already sent, so the only honest signal left is to break the connection:
		// the client then sees a failed download instead of a file that silently ends early.
		slog.Error("report stream failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		panic(http.ErrAbortHandler)
	}
}

func (h *Handler) pdf(w http.ResponseWriter, r *http.Request) {
	p, ok := parse(w, r)
	if !ok {
		return
	}
	actor, _ := auth.PrincipalFrom(r.Context())
	n, err := h.svc.Count(r.Context(), actor, p)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if n > MaxPDFRows {
		httpx.ValidationError(w, map[string]string{
			"from": "Terlalu banyak data untuk satu PDF (" + strconv.Itoa(n) + " baris, maksimal " + strconv.Itoa(MaxPDFRows) +
				"). Persempit periode atau unduh CSV.",
		})
		return
	}
	if err := h.svc.Audit(r.Context(), actor, "pdf", p, n); err != nil {
		h.serverError(w, r, err)
		return
	}

	rows := make([]Row, 0, n)
	if err := h.svc.Each(r.Context(), actor, p, func(page []Row) error {
		rows = append(rows, page...)
		return nil
	}); err != nil {
		h.serverError(w, r, err)
		return
	}
	doc, err := buildPDF(rows, p, h.svc.now().In(httpx.WIB))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/pdf")
	hd.Set("Content-Disposition", `attachment; filename="`+filename(p, "pdf")+`"`)
	hd.Set("Content-Length", strconv.Itoa(len(doc)))
	_, _ = bytes.NewReader(doc).WriteTo(w)
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("report request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
	httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
}
