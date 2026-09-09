// Package httpx holds the JSON transport helpers shared by the Lunex resource
// modules (tasks, goals, notes).
//
// The error wire types are aliases of the ones internal/auth already defines,
// not copies: every endpoint in the API returns exactly one error shape, and an
// alias makes that a compile-time fact rather than a convention two packages
// have to keep agreeing on.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/auth"
)

// MaxBodyBytes caps request bodies. Note content is the largest thing a client
// can send in Phase 2 and it is capped well below this by validation.
const MaxBodyBytes = 64 << 10

// TimeFormat matches the one auth uses, so timestamps look the same everywhere.
const TimeFormat = "2006-01-02T15:04:05.000Z"

type (
	// ErrorBody is the single error shape every endpoint returns.
	ErrorBody = auth.ErrorBody
	// ValidationError describes a single rejected field.
	ValidationError = auth.ValidationError
	// ValidationErrors is the multi-field validation failure returned by services.
	ValidationErrors = auth.ValidationErrors
)

// WriteJSON encodes body as the response. A nil body writes only the status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

// WriteError writes the standard error body.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message})
}

// NotFound is the one response for "no such row" and for "that row is not
// yours". Distinguishing them would tell a caller which ids exist.
func NotFound(w http.ResponseWriter, resource string) {
	WriteError(w, http.StatusNotFound, "not_found", "No such "+resource+".")
}

// DecodeJSON reads a strict JSON body: unknown fields, trailing content and
// oversized payloads are all rejected so a typo'd field never silently becomes
// a default. It writes the error response itself and reports whether decoding
// succeeded.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt := strings.TrimSpace(strings.Split(ct, ";")[0]); mt != "application/json" {
			WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
			return false
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large.")
			return false
		}
		WriteError(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		WriteError(w, http.StatusBadRequest, "invalid_json", "Request body must contain a single JSON object.")
		return false
	}
	return true
}

// UserID returns the authenticated user placed on the context by
// auth.RequireAuth. Handlers behind that middleware can rely on ok being true;
// the check exists so a future route mounted outside it fails closed.
func UserID(ctx context.Context) (uuid.UUID, bool) {
	return auth.UserIDFromContext(ctx)
}
