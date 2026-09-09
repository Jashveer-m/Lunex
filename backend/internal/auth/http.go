package auth

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxBodyBytes caps request bodies; auth payloads are tiny.
const maxBodyBytes = 64 << 10

// ErrorBody is the single error shape every endpoint returns.
type ErrorBody struct {
	Error   string            `json:"error"`
	Message string            `json:"message"`
	Fields  []ValidationError `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorBody{Error: code, Message: message})
}

// decodeJSON reads a strict JSON body: unknown fields and trailing content are
// rejected so a typo'd field never silently becomes a default.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt := strings.TrimSpace(strings.Split(ct, ";")[0]); mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large.")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "Request body must contain a single JSON object.")
		return false
	}
	return true
}
