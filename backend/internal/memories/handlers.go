package memories

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Routes returns the subtree mounted at /memories. It assumes
// auth.RequireAuth is already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Delete("/", h.Clear)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	return r
}

// --- wire types -------------------------------------------------------------

type memoryPatchRequest struct {
	Content optional.Field[string] `json:"content"`
	Enabled optional.Field[bool]   `json:"enabled"`
}

// clearRequest is the body of DELETE /memories. The flag is required and must
// be true: an accidental DELETE on the collection has to be indistinguishable
// from a mistake, not from an instruction.
type clearRequest struct {
	Confirm bool `json:"confirm"`
}

type memoryResponse struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Content    string  `json:"content"`
	Importance float64 `json:"importance"`
	Confidence float64 `json:"confidence"`
	// SourceConversationID is null once the conversation it came from has been
	// deleted: the fact outlives its source, and says so.
	SourceConversationID *string `json:"source_conversation_id"`
	Enabled              bool    `json:"enabled"`
	ExpiresAt            *string `json:"expires_at"`
	CreatedAt            string  `json:"created_at"`
	UpdatedAt            string  `json:"updated_at"`
}

type listResponse struct {
	Memories []memoryResponse `json:"memories"`
	Count    int              `json:"count"`
	Limit    int              `json:"limit"`
	Offset   int              `json:"offset"`
}

type clearResponse struct {
	Deleted int `json:"deleted"`
}

func toResponse(m Memory) memoryResponse {
	out := memoryResponse{
		ID:      m.ID.String(),
		Type:    m.Type,
		Content: m.Content,
		// Rounded because the column is `real`: single precision turns 0.7 into
		// 0.699999988079071 on the way back, and a score the model gave to one
		// decimal place should not be reported to fifteen.
		Importance: round2(m.Importance),
		Confidence: round2(m.Confidence),
		Enabled:    m.Enabled,
		CreatedAt:  m.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:  m.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if m.SourceConversationID != nil {
		id := m.SourceConversationID.String()
		out.SourceConversationID = &id
	}
	if m.ExpiresAt != nil {
		at := m.ExpiresAt.UTC().Format(httpx.TimeFormat)
		out.ExpiresAt = &at
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/memories.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Type: q.Get("type"), Sort: q.Get("sort")}
	var err error
	if filter.Enabled, err = boolParam(q.Get("enabled")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "enabled must be true or false.")
		return
	}
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	found, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	effective, _ := ValidateFilter(filter)
	out := listResponse{
		Memories: make([]memoryResponse, 0, len(found)),
		Count:    len(found), Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, m := range found {
		out.Memories = append(out.Memories, toResponse(m))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Update handles PATCH /api/v1/memories/{id}: correct a fact, or switch it off
// without losing it.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req memoryPatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.Update(r.Context(), userID, id, UpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(m))
}

// Delete handles DELETE /api/v1/memories/{id}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Clear handles DELETE /api/v1/memories: forget everything about the caller.
//
// It answers 200 with a count rather than 204. The count is the only way the
// caller can tell "there was nothing to forget" from "four hundred facts are
// gone", and this is a request nobody gets to make twice.
func (h *Handler) Clear(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req clearRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	deleted, err := h.svc.Clear(r.Context(), userID, req.Confirm)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, clearResponse{Deleted: deleted})
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a memory belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "memory")
		return uuid.Nil, uuid.Nil, false
	}
	return userID, id, true
}

func intParam(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// boolParam reads the three-state ?enabled= filter: absent lists everything.
func boolParam(v string) (*bool, error) {
	if v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verrs httpx.ValidationErrors
	switch {
	case errors.As(err, &verrs):
		httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
			Error:   "validation_failed",
			Message: "One or more fields are invalid.",
			Fields:  verrs,
		})
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, "memory")
	case errors.Is(err, embeddings.ErrUnavailable) || errors.Is(err, ErrEmbedding):
		// Editing a memory's text means re-embedding it, and that cannot happen
		// without the embedding service. It is an outage, not a bad request:
		// 503 is the code a client should retry on.
		h.log.Error("memory embedding unavailable", "error", err, "path", r.URL.Path)
		httpx.WriteError(w, http.StatusServiceUnavailable, "embedding_unavailable",
			"The memory could not be re-indexed because the embedding service could not be reached.")
	default:
		h.log.Error("memory request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
