package stats

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ghofinka/go-housing-guest-book/internal/auth"
	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Handler exposes GET /v1/stats/visits to every signed-in role. The answer is scoped by role (see the
// package comment) and holds counts only, so a client can load it as soon as the user logs in.
type Handler struct {
	svc   *Service
	authn *auth.Authenticator
}

func NewHandler(svc *Service, authn *auth.Authenticator) *Handler {
	return &Handler{svc: svc, authn: authn}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("GET /v1/stats/visits", h.authn.Require(http.HandlerFunc(h.visits), auth.RoleOwner, auth.RoleGuard, auth.RoleAdmin))
}

// parse reads the query. from and to are optional so a client can show the page at once: with neither, the
// period is today and the six days before it (WIB). It returns false when it already answered with a 422.
func (h *Handler) parse(w http.ResponseWriter, r *http.Request) (Params, bool) {
	q := r.URL.Query()
	fields := map[string]string{}
	var pr Params

	now := h.svc.now().In(httpx.WIB)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, httpx.WIB)
	hasFrom, hasTo := q.Get("from") != "", q.Get("to") != ""
	if hasTo {
		if d, ok := httpx.ParseDay(q.Get("to"), "to", fields); ok {
			pr.To = d
		}
	} else {
		pr.To = today
	}
	if hasFrom {
		if d, ok := httpx.ParseDay(q.Get("from"), "from", fields); ok {
			pr.From = d
		}
	} else if !pr.To.IsZero() {
		pr.From = pr.To.AddDate(0, 0, -(DefaultDays - 1))
	}

	if fields["from"] == "" && fields["to"] == "" {
		switch {
		case pr.To.Before(pr.From):
			fields["to"] = "Tanggal akhir tidak boleh sebelum tanggal awal."
		case pr.To.Sub(pr.From) > time.Duration(MaxPeriodDays-1)*24*time.Hour+time.Hour:
			fields["to"] = "Periode maksimal " + strconv.Itoa(MaxPeriodDays) + " hari."
		}
	}
	if v := q.Get("house_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			fields["house_id"] = "house_id tidak valid."
		}
		pr.HouseID = &id
	}
	if len(fields) > 0 {
		httpx.ValidationError(w, fields)
		return Params{}, false
	}
	return pr, true
}

func (h *Handler) visits(w http.ResponseWriter, r *http.Request) {
	pr, ok := h.parse(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := h.svc.Get(r.Context(), p, pr)
	if err != nil {
		slog.Error("statistics request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		httpx.Error(w, http.StatusInternalServerError, "internal_error", "Terjadi kesalahan. Coba lagi.")
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}
