// Package server wires routes and middleware into an http.Handler.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/ghofinka/go-housing-guest-book/internal/httpx"
)

// Pinger reports whether a dependency (the database) is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Registrar mounts a feature's routes on the mux.
type Registrar interface {
	Register(mux *http.ServeMux)
}

// Deps are the collaborators wired in main.
type Deps struct {
	DB         Pinger
	CORSOrigin string
	Routes     []Registrar
}

func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "db_unavailable", "Database belum siap.")
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	for _, r := range d.Routes {
		r.Register(mux)
	}

	return httpx.Middleware(httpx.CORS(d.CORSOrigin, mux))
}
