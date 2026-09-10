package tasks

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

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

// Routes returns the subtree mounted at /tasks. It assumes auth.RequireAuth is
// already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	r.Post("/{id}/dependencies", h.AddDependency)
	return r
}

// --- wire types -------------------------------------------------------------

type taskRequest struct {
	Title                  string     `json:"title"`
	Description            string     `json:"description"`
	Priority               string     `json:"priority"`
	Status                 string     `json:"status"`
	Category               string     `json:"category"`
	Tags                   []string   `json:"tags"`
	Deadline               *time.Time `json:"deadline"`
	ParentTaskID           *uuid.UUID `json:"parent_task_id"`
	EstimatedEffortMinutes *int       `json:"estimated_effort_minutes"`
	ActualEffortMinutes    *int       `json:"actual_effort_minutes"`
}

type taskPatchRequest struct {
	Title                  optional.Field[string]    `json:"title"`
	Description            optional.Field[string]    `json:"description"`
	Priority               optional.Field[string]    `json:"priority"`
	Status                 optional.Field[string]    `json:"status"`
	Category               optional.Field[string]    `json:"category"`
	Tags                   optional.Field[[]string]  `json:"tags"`
	Deadline               optional.Field[time.Time] `json:"deadline"`
	ParentTaskID           optional.Field[uuid.UUID] `json:"parent_task_id"`
	EstimatedEffortMinutes optional.Field[int]       `json:"estimated_effort_minutes"`
	ActualEffortMinutes    optional.Field[int]       `json:"actual_effort_minutes"`
}

type dependencyRequest struct {
	DependsOnTaskID uuid.UUID `json:"depends_on_task_id"`
}

type taskResponse struct {
	ID                     string   `json:"id"`
	Title                  string   `json:"title"`
	Description            *string  `json:"description"`
	Priority               string   `json:"priority"`
	Status                 string   `json:"status"`
	Category               *string  `json:"category"`
	Tags                   []string `json:"tags"`
	Deadline               *string  `json:"deadline"`
	ParentTaskID           *string  `json:"parent_task_id"`
	EstimatedEffortMinutes *int     `json:"estimated_effort_minutes"`
	ActualEffortMinutes    *int     `json:"actual_effort_minutes"`
	DependsOn              []string `json:"depends_on"`
	CreatedAt              string   `json:"created_at"`
	UpdatedAt              string   `json:"updated_at"`
}

type listResponse struct {
	Tasks  []taskResponse `json:"tasks"`
	Count  int            `json:"count"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func toResponse(t Task) taskResponse {
	resp := taskResponse{
		ID:                     t.ID.String(),
		Title:                  t.Title,
		Description:            t.Description,
		Priority:               t.Priority,
		Status:                 t.Status,
		Category:               t.Category,
		Tags:                   t.Tags,
		EstimatedEffortMinutes: t.EstimatedEffortMinutes,
		ActualEffortMinutes:    t.ActualEffortMinutes,
		DependsOn:              []string{},
		CreatedAt:              t.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:              t.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	// An absent list is [] rather than null: clients should not have to
	// special-case an empty array.
	if resp.Tags == nil {
		resp.Tags = []string{}
	}
	if t.Deadline != nil {
		d := t.Deadline.UTC().Format(httpx.TimeFormat)
		resp.Deadline = &d
	}
	if t.ParentTaskID != nil {
		p := t.ParentTaskID.String()
		resp.ParentTaskID = &p
	}
	for _, id := range t.DependsOn {
		resp.DependsOn = append(resp.DependsOn, id.String())
	}
	return resp
}

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/tasks.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{
		Status:   q.Get("status"),
		Category: q.Get("category"),
		Tag:      q.Get("tag"),
		Query:    q.Get("q"),
		Sort:     q.Get("sort"),
	}
	var err error
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	tasks, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	// Echo back the effective paging, which may have been clamped.
	effective, _ := ValidateFilter(filter)
	out := listResponse{Tasks: make([]taskResponse, 0, len(tasks)), Count: len(tasks), Limit: effective.Limit, Offset: effective.Offset}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, toResponse(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create handles POST /api/v1/tasks.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req taskRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	task, err := h.svc.Create(r.Context(), userID, CreateInput{
		Title:                  req.Title,
		Description:            req.Description,
		Priority:               req.Priority,
		Status:                 req.Status,
		Category:               req.Category,
		Tags:                   req.Tags,
		Deadline:               req.Deadline,
		ParentTaskID:           req.ParentTaskID,
		EstimatedEffortMinutes: req.EstimatedEffortMinutes,
		ActualEffortMinutes:    req.ActualEffortMinutes,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(task))
}

// Get handles GET /api/v1/tasks/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	task, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(task))
}

// Update handles PATCH /api/v1/tasks/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req taskPatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	task, err := h.svc.Update(r.Context(), userID, id, UpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(task))
}

// Delete handles DELETE /api/v1/tasks/{id}.
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

// AddDependency handles POST /api/v1/tasks/{id}/dependencies.
func (h *Handler) AddDependency(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req dependencyRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	task, err := h.svc.AddDependency(r.Context(), userID, id, req.DependsOnTaskID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(task))
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id is a 404
// rather than a 400: "that is not a task of yours" is true either way, and one
// answer for both means the endpoint reveals nothing about id validity.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "task")
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
		httpx.NotFound(w, "task")
	case errors.Is(err, ErrDependencyCycle):
		httpx.WriteError(w, http.StatusConflict, "dependency_cycle", "That dependency would create a cycle.")
	default:
		// Internal detail stays in the log, never in the response.
		h.log.Error("task request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
