package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A handler behind Middleware must still be able to flush a streamed response: the middleware wraps
// the writer to read the status, and without Unwrap the wrapper hides the real writer's Flush.
func TestMiddlewareLetsAHandlerFlush(t *testing.T) {
	var flushErr error
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first page"))
		flushErr = http.NewResponseController(w).Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if flushErr != nil {
		t.Fatalf("flush through the middleware failed: %v", flushErr)
	}
	if !rec.Flushed {
		t.Fatal("the flush never reached the underlying writer")
	}
}

// A browser on another origin can only read a download's file name if it is exposed.
func TestCORSExposesTheDownloadFileName(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS("https://app.example.test", next)

	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/v1/reports/visits.csv", nil))
		if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "Content-Disposition" {
			t.Errorf("%s: Access-Control-Expose-Headers = %q, want Content-Disposition", method, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.test" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q", method, got)
		}
	}

	// With no origin configured, CORS is off and nothing is added.
	rec := httptest.NewRecorder()
	CORS("", next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Access-Control-Expose-Headers") != "" || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS headers were set although no origin is configured")
	}
}
