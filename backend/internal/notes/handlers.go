package notes

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

// Routes returns the subtree mounted at /notes. It assumes auth.RequireAuth is
// already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	return r
}

// --- wire types -------------------------------------------------------------

type noteRequest struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
}

type notePatchRequest struct {
	Title   optional.Field[string]   `json:"title"`
	Content optional.Field[string]   `json:"content"`
	Tags    optional.Field[[]string] `json:"tags"`
}

type noteResponse struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type listResponse struct {
	Notes  []noteResponse `json:"notes"`
	Count  int            `json:"count"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func toResponse(n Note) noteResponse {
	tags := n.Tags
	// An absent list is [] rather than null so clients need no special case.
	if tags == nil {
		tags = []string{}
	}
	return noteResponse{
		ID:        n.ID.String(),
		Title:     n.Title,
		Content:   n.Content,
		Tags:      tags,
		CreatedAt: n.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt: n.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
}

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/notes.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Tag: q.Get("tag"), Query: q.Get("q"), Sort: q.Get("sort")}
	var err error
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
	out := listResponse{Notes: make([]noteResponse, 0, len(found)), Count: len(found), Limit: effective.Limit, Offset: effective.Offset}
	for _, n := range found {
		out.Notes = append(out.Notes, toResponse(n))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create handles POST /api/v1/notes.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req noteRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	note, err := h.svc.Create(r.Context(), userID, CreateInput{
		Title: req.Title, Content: req.Content, Tags: req.Tags,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(note))
}

// Get handles GET /api/v1/notes/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	note, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(note))
}

// Update handles PATCH /api/v1/notes/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req notePatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	note, err := h.svc.Update(r.Context(), userID, id, UpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(note))
}

// Delete handles DELETE /api/v1/notes/{id}.
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

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a note belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "note")
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
		httpx.NotFound(w, "note")
	default:
		h.log.Error("note request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
