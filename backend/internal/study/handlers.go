package study

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

// PlanRoutes returns the subtree mounted at /study-plans. It assumes
// auth.RequireAuth is already in front of it.
//
// There is no POST that generates cards. Generation costs a model call and
// produces content the user has to read before it is theirs, which is what the
// approval flow is for -- so it is reachable through the assistant
// (generate_flashcards) and not as a fire-and-forget endpoint. POST
// /{id}/flashcards below is the direct path, and it takes a card the user
// wrote.
func (h *Handler) PlanRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{id}", h.Get)
	r.Patch("/{id}", h.Update)
	r.Delete("/{id}", h.Delete)
	r.Get("/{id}/flashcards", h.ListFlashcards)
	r.Post("/{id}/flashcards", h.AddFlashcard)
	return r
}

// FlashcardRoutes returns the subtree mounted at /flashcards.
//
// DELETE only, and that is the whole of it this phase. A card has no PATCH --
// there is nothing to change on it that is not "write a different card" --
// and there is no unfiltered GET /flashcards, because the only slice of a deck
// anybody wants is one plan's, which PlanRoutes answers. A card lives at a
// top-level path all the same: it can belong to no plan, so it cannot be
// addressed only as a child of one.
func (h *Handler) FlashcardRoutes() chi.Router {
	r := chi.NewRouter()
	r.Delete("/{id}", h.DeleteFlashcard)
	return r
}

// --- wire types -------------------------------------------------------------

type planRequest struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	DocumentID  *uuid.UUID `json:"document_id"`
	Status      string     `json:"status"`
}

type planPatchRequest struct {
	Title       optional.Field[string]    `json:"title"`
	Description optional.Field[string]    `json:"description"`
	DocumentID  optional.Field[uuid.UUID] `json:"document_id"`
	Status      optional.Field[string]    `json:"status"`
}

type planResponse struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	// Document is the filename, beside the id, so a client renders a row
	// without a second request. Both are null for a plan built from no
	// document, and become null when the document is deleted.
	DocumentID *string `json:"document_id"`
	Document   *string `json:"document"`
	Status     string  `json:"status"`
	CardCount  int     `json:"card_count"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

type planListResponse struct {
	StudyPlans []planResponse `json:"study_plans"`
	Count      int            `json:"count"`
	Limit      int            `json:"limit"`
	Offset     int            `json:"offset"`
}

type cardRequest struct {
	Front string `json:"front"`
	Back  string `json:"back"`
}

type cardResponse struct {
	ID          string  `json:"id"`
	StudyPlanID *string `json:"study_plan_id"`
	// DocumentID is where the card came from: the document for a generated
	// card, null for one the user typed. It is the card's provenance, so it is
	// on the wire rather than kept internal.
	DocumentID *string `json:"document_id"`
	Front      string  `json:"front"`
	Back       string  `json:"back"`
	CreatedAt  string  `json:"created_at"`
}

type cardListResponse struct {
	Flashcards []cardResponse `json:"flashcards"`
	Count      int            `json:"count"`
	Limit      int            `json:"limit"`
	Offset     int            `json:"offset"`
}

func toPlanResponse(p Plan) planResponse {
	out := planResponse{
		ID: p.ID.String(), Title: p.Title, Description: p.Description,
		Document: p.DocumentName, Status: p.Status, CardCount: p.CardCount,
		CreatedAt: p.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt: p.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if p.DocumentID != nil {
		id := p.DocumentID.String()
		out.DocumentID = &id
	}
	return out
}

func toCardResponse(c Flashcard) cardResponse {
	out := cardResponse{
		ID: c.ID.String(), Front: c.Front, Back: c.Back,
		CreatedAt: c.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
	if c.StudyPlanID != nil {
		id := c.StudyPlanID.String()
		out.StudyPlanID = &id
	}
	if c.DocumentID != nil {
		id := c.DocumentID.String()
		out.DocumentID = &id
	}
	return out
}

// --- plan handlers ------------------------------------------------------------

// List handles GET /api/v1/study-plans?status=&document_id=&q=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	q := r.URL.Query()
	filter := Filter{Status: q.Get("status"), Query: q.Get("q"), Sort: q.Get("sort")}
	if raw := q.Get("document_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "document_id must be an id.")
			return
		}
		filter.DocumentID = &id
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

	found, err := h.svc.Plans(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	// Echo back the effective paging, which may have been clamped.
	effective, _ := ValidateFilter(filter)
	out := planListResponse{
		StudyPlans: make([]planResponse, 0, len(found)), Count: len(found),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, p := range found {
		out.StudyPlans = append(out.StudyPlans, toPlanResponse(p))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Create handles POST /api/v1/study-plans.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req planRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	plan, err := h.svc.CreatePlan(r.Context(), userID, CreatePlanInput{
		Title: req.Title, Description: req.Description,
		DocumentID: req.DocumentID, Status: req.Status,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toPlanResponse(plan))
}

// Get handles GET /api/v1/study-plans/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "study plan")
	if !ok {
		return
	}
	plan, err := h.svc.Plan(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toPlanResponse(plan))
}

// Update handles PATCH /api/v1/study-plans/{id}.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "study plan")
	if !ok {
		return
	}
	var req planPatchRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	plan, err := h.svc.UpdatePlan(r.Context(), userID, id, UpdatePlanInput{
		Title: req.Title, Description: req.Description,
		DocumentID: req.DocumentID, Status: req.Status,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toPlanResponse(plan))
}

// Delete handles DELETE /api/v1/study-plans/{id}. The plan's cards go with it.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "study plan")
	if !ok {
		return
	}
	if err := h.svc.DeletePlan(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- flashcard handlers --------------------------------------------------------

// ListFlashcards handles GET /api/v1/study-plans/{id}/flashcards.
func (h *Handler) ListFlashcards(w http.ResponseWriter, r *http.Request) {
	userID, planID, ok := h.scope(w, r, "study plan")
	if !ok {
		return
	}
	q := r.URL.Query()
	var filter CardFilter
	var err error
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	found, err := h.svc.Flashcards(r.Context(), userID, planID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	effective, _ := ValidateCardFilter(filter)
	out := cardListResponse{
		Flashcards: make([]cardResponse, 0, len(found)), Count: len(found),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, c := range found {
		out.Flashcards = append(out.Flashcards, toCardResponse(c))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// AddFlashcard handles POST /api/v1/study-plans/{id}/flashcards.
//
// Direct, with no approval: the user is the one writing the card. Approval
// stands between the assistant and the user's data, not between the user and
// their own.
func (h *Handler) AddFlashcard(w http.ResponseWriter, r *http.Request) {
	userID, planID, ok := h.scope(w, r, "study plan")
	if !ok {
		return
	}
	var req cardRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	card, err := h.svc.AddFlashcard(r.Context(), userID, planID, NewCard{Front: req.Front, Back: req.Back})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toCardResponse(card))
}

// DeleteFlashcard handles DELETE /api/v1/flashcards/{id}.
func (h *Handler) DeleteFlashcard(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r, "flashcard")
	if !ok {
		return
	}
	if err := h.svc.DeleteFlashcard(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id is a 404
// rather than a 400, for the reason tasks.Handler.scope gives.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request, resource string) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, resource)
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
	case errors.Is(err, ErrFlashcardNotFound):
		httpx.NotFound(w, "flashcard")
	// Phase 10b. Each names its own resource, so "no such quiz" and "no such
	// attempt" are different sentences -- and all of them are 404s, including
	// the ones that mean "that is somebody else's".
	case errors.Is(err, ErrQuizNotFound):
		httpx.NotFound(w, "quiz")
	case errors.Is(err, ErrAttemptNotFound):
		httpx.NotFound(w, "quiz attempt")
	case errors.Is(err, ErrQuestionNotFound):
		httpx.NotFound(w, "question in this quiz")
	// The two conflicts. They are 409 rather than 400 because the request is
	// well formed and was legal a moment ago: what is wrong is the state of
	// the attempt, not the body.
	case errors.Is(err, ErrAlreadyAnswered):
		httpx.WriteError(w, http.StatusConflict, "already_answered",
			"That question has already been answered in this attempt.")
	case errors.Is(err, ErrAttemptComplete):
		httpx.WriteError(w, http.StatusConflict, "attempt_complete",
			"This attempt is already complete.")
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, "study plan")
	default:
		// Internal detail stays in the log, never in the response.
		h.log.Error("study request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
