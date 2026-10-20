// Package httpx holds shared HTTP helpers: JSON responses, the error envelope,
// request decoding and common middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// maxBodyBytes caps JSON request bodies.
const maxBodyBytes = 1 << 20

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

// Error writes the standard error envelope. message is user-facing (Indonesian).
func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// ValidationError writes a 422 with field-level details (field -> message).
func ValidationError(w http.ResponseWriter, fields map[string]string) {
	JSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{
		Code:    "validation_failed",
		Message: "Data yang dikirim belum benar.",
		Details: fields,
	}})
}

// DecodeJSON reads a size-capped JSON body into dst and rejects unknown fields
// and trailing data. On failure it writes the error response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		Error(w, http.StatusBadRequest, "invalid_json", "Format data tidak valid.")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, "invalid_json", "Format data tidak valid.")
		return false
	}
	return true
}
