package goals

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

// Routes returns the subtree mounted at /goals. It assumes auth.RequireAuth is
// already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	r.Post("/{id}/milestones", h.AddMilestone)
	r.Patch("/{id}/milestones/{milestone_id}", h.UpdateMilestone)
	return r
}

// --- wire types -------------------------------------------------------------

type goalRequest struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	Deadline    *time.Time `json:"deadline"`
}

type goalPatchRequest struct {
	Title       optional.Field[string]    `json:"title"`
	Description optional.Field[string]    `json:"description"`
	Type        optional.Field[string]    `json:"type"`
	Status      optional.Field[string]    `json:"status"`
	Deadline    optional.Field[time.Time] `json:"deadline"`
}

type milestoneRequest struct {
	Title      string     `json:"title"`
	TargetDate *time.Time `json:"target_date"`
}

type milestonePatchRequest struct {
	Title      optional.Field[string]    `json:"title"`
	TargetDate optional.Field[time.Time] `json:"target_date"`
	Completed  optional.Field[bool]      `json:"completed"`
}

type milestoneResponse struct {
	ID         string  `json:"id"`
	GoalID     string  `json:"goal_id"`
	Title      string  `json:"title"`
	TargetDate *string `json:"target_date"`
	Completed  bool    `json:"completed"`
	CreatedAt  string  `json:"created_at"`
}

type goalResponse struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	Description *string             `json:"description"`
	Type        string              `json:"type"`
	Status      string              `json:"status"`
	Deadline    *string             `json:"deadline"`
	Milestones  []milestoneResponse `json:"milestones"`
	CreatedAt   string              `json:"created_at"`
	UpdatedAt   string              `json:"updated_at"`
}

type listResponse struct {
	Goals  []goalResponse `json:"goals"`
	Count  int            `json:"count"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func toMilestoneResponse(m Milestone) milestoneResponse {
	resp := milestoneResponse{
		ID:        m.ID.String(),
		GoalID:    m.GoalID.String(),
		Title:     m.Title,
		Completed: m.Completed,
		CreatedAt: m.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
	if m.TargetDate != nil {
		d := m.TargetDate.UTC().Format(httpx.TimeFormat)
		resp.TargetDate = &d
	}
	return resp
}

func toResponse(g Goal) goalResponse {
	resp := goalResponse{
		ID:          g.ID.String(),
		Title:       g.Title,
		Description: g.Description,
		Type:        g.Type,
		Status:      g.Status,
		// An absent list is [] rather than null so clients need no special case.
		Milestones: make([]milestoneResponse, 0, len(g.Milestones)),
		CreatedAt:  g.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:  g.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if g.Deadline != nil {
		d := g.Deadline.UTC().Format(httpx.TimeFormat)
		resp.Deadline = &d
	}
	for _, m := range g.Milestones {
		resp.Milestones = append(resp.Milestones, toMilestoneResponse(m))
	}
	return resp
}

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/goals.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Status: q.Get("status"), Type: q.Get("type"), Sort: q.Get("sort")}
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
	out := listResponse{Goals: make([]goalResponse, 0, len(found)), Count: len(found), Limit: effective.Limit, Offset: effective.Offset}
	for _, g := range found {
		out.Goals = append(out.Goals, toResponse(g))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create handles POST /api/v1/goals.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req goalRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	goal, err := h.svc.Create(r.Context(), userID, CreateInput{
		Title:       req.Title,
		Description: req.Description,
		Type:        req.Type,
		Status:      req.Status,
		Deadline:    req.Deadline,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(goal))
}

// Get handles GET /api/v1/goals/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	goal, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(goal))
}

// Update handles PATCH /api/v1/goals/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req goalPatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	goal, err := h.svc.Update(r.Context(), userID, id, UpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(goal))
}

// Delete handles DELETE /api/v1/goals/{id}.
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

// AddMilestone handles POST /api/v1/goals/{id}/milestones.
func (h *Handler) AddMilestone(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req milestoneRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.AddMilestone(r.Context(), userID, id, MilestoneInput{Title: req.Title, TargetDate: req.TargetDate})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toMilestoneResponse(m))
}

// UpdateMilestone handles PATCH /api/v1/goals/{id}/milestones/{milestone_id}.
func (h *Handler) UpdateMilestone(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	milestoneID, err := uuid.Parse(chi.URLParam(r, "milestone_id"))
	if err != nil {
		httpx.NotFound(w, "milestone")
		return
	}
	var req milestonePatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.UpdateMilestone(r.Context(), userID, id, milestoneID, MilestoneUpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toMilestoneResponse(m))
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a goal belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "goal")
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
		httpx.NotFound(w, "goal")
	case errors.Is(err, ErrMilestoneNotFound):
		httpx.NotFound(w, "milestone")
	default:
		h.log.Error("goal request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
