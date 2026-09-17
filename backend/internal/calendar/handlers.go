package calendar

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

// Routes returns the subtree mounted at /calendar. It assumes auth.RequireAuth
// is already in front of it.
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

type eventRequest struct {
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	StartTime      *time.Time `json:"start_time"`
	EndTime        *time.Time `json:"end_time"`
	AllDay         bool       `json:"all_day"`
	Location       string     `json:"location"`
	RecurrenceRule string     `json:"recurrence_rule"`
	RelatedTaskID  *uuid.UUID `json:"related_task_id"`
	RelatedGoalID  *uuid.UUID `json:"related_goal_id"`
}

type eventPatchRequest struct {
	Title          optional.Field[string]    `json:"title"`
	Description    optional.Field[string]    `json:"description"`
	StartTime      optional.Field[time.Time] `json:"start_time"`
	EndTime        optional.Field[time.Time] `json:"end_time"`
	AllDay         optional.Field[bool]      `json:"all_day"`
	Location       optional.Field[string]    `json:"location"`
	RecurrenceRule optional.Field[string]    `json:"recurrence_rule"`
	RelatedTaskID  optional.Field[uuid.UUID] `json:"related_task_id"`
	RelatedGoalID  optional.Field[uuid.UUID] `json:"related_goal_id"`
}

type eventResponse struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Description    *string `json:"description"`
	StartTime      string  `json:"start_time"`
	EndTime        string  `json:"end_time"`
	AllDay         bool    `json:"all_day"`
	Location       *string `json:"location"`
	RecurrenceRule *string `json:"recurrence_rule"`
	RelatedTaskID  *string `json:"related_task_id"`
	RelatedGoalID  *string `json:"related_goal_id"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

// listResponse echoes the window back as well as the paging, so a client can
// see exactly which range answered -- the parameters are required and parsed,
// and a range that was not what the caller meant should be visible in the
// response rather than inferred from the events in it.
type listResponse struct {
	Events []eventResponse `json:"events"`
	Count  int             `json:"count"`
	Start  string          `json:"start"`
	End    string          `json:"end"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func toResponse(e Event) eventResponse {
	resp := eventResponse{
		ID:             e.ID.String(),
		Title:          e.Title,
		Description:    e.Description,
		StartTime:      e.StartTime.UTC().Format(httpx.TimeFormat),
		EndTime:        e.EndTime.UTC().Format(httpx.TimeFormat),
		AllDay:         e.AllDay,
		Location:       e.Location,
		RecurrenceRule: e.RecurrenceRule,
		CreatedAt:      e.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:      e.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if e.RelatedTaskID != nil {
		id := e.RelatedTaskID.String()
		resp.RelatedTaskID = &id
	}
	if e.RelatedGoalID != nil {
		id := e.RelatedGoalID.String()
		resp.RelatedGoalID = &id
	}
	return resp
}

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/calendar?start=&end=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Query: q.Get("q"), Sort: q.Get("sort")}
	var err error
	if filter.Start, err = timeParam(q.Get("start")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
			"start must be an RFC 3339 timestamp or a YYYY-MM-DD date.")
		return
	}
	if filter.End, err = timeParam(q.Get("end")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed",
			"end must be an RFC 3339 timestamp or a YYYY-MM-DD date.")
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

	events, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	// Echo back the effective query, which may have been clamped.
	effective, _ := ValidateFilter(filter)
	out := listResponse{
		Events: make([]eventResponse, 0, len(events)), Count: len(events),
		Start: effective.Start.UTC().Format(httpx.TimeFormat),
		End:   effective.End.UTC().Format(httpx.TimeFormat),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, e := range events {
		out.Events = append(out.Events, toResponse(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create handles POST /api/v1/calendar.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req eventRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	in := CreateInput{
		Title: req.Title, Description: req.Description, AllDay: req.AllDay,
		Location: req.Location, RecurrenceRule: req.RecurrenceRule,
		RelatedTaskID: req.RelatedTaskID, RelatedGoalID: req.RelatedGoalID,
	}
	if req.StartTime != nil {
		in.StartTime = *req.StartTime
	}
	if req.EndTime != nil {
		in.EndTime = *req.EndTime
	}

	event, err := h.svc.Create(r.Context(), userID, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(event))
}

// Get handles GET /api/v1/calendar/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	event, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(event))
}

// Update handles PATCH /api/v1/calendar/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req eventPatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	event, err := h.svc.Update(r.Context(), userID, id, UpdateInput(req))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(event))
}

// Delete handles DELETE /api/v1/calendar/{id}.
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

// scope pulls the authenticated user and the path id. A malformed id is a 404
// rather than a 400, for the reason tasks.Handler.scope gives.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "event")
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

// timeParam reads a window bound. A bare date is accepted as midnight UTC on
// that day, because `?start=2026-09-17&end=2026-09-24` is what a calendar
// client actually asks for and spelling it in full adds nothing.
//
// An empty value is the zero time, which ValidateFilter reports as the missing
// required parameter it is -- so "no start" and "an unreadable start" stay
// different answers.
func timeParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse(time.DateOnly, v)
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
		httpx.NotFound(w, "event")
	default:
		// Internal detail stays in the log, never in the response.
		h.log.Error("calendar request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
