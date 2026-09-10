package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Routes returns the subtree mounted at /actions. It assumes auth.RequireAuth
// is already in front of it.
//
// There is no POST to create an action. Actions come from the assistant's turn
// -- a proposal is something the assistant asks the user, not something a
// client files -- and a client that wants to create a task already has
// POST /tasks, which needs no approval because the user is the one asking.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Get("/{id}", h.Get)
	r.Post("/{id}/approve", h.Approve)
	r.Post("/{id}/reject", h.Reject)
	return r
}

// --- wire types -------------------------------------------------------------

// Response is an action on the wire. It is exported because the chat stream
// announces the actions a turn produced in exactly this shape: one
// representation of an action, wherever a client meets it.
type Response struct {
	ID             string  `json:"id"`
	ConversationID *string `json:"conversation_id"`
	ToolName       string  `json:"tool_name"`
	// PermissionLevel is "read" or "write". Only a write can ever be proposed.
	PermissionLevel string          `json:"permission_level"`
	Status          string          `json:"status"`
	Input           json.RawMessage `json:"input"`
	// Summary is one sentence saying what the action does, derived from Input
	// every time it is read -- never stored, so it cannot drift from what would
	// actually run.
	Summary      string          `json:"summary"`
	Result       json.RawMessage `json:"result"`
	ErrorMessage *string         `json:"error_message"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

type listResponse struct {
	Actions []Response `json:"actions"`
	Count   int        `json:"count"`
	Limit   int        `json:"limit"`
	Offset  int        `json:"offset"`
}

// ToResponse renders an action with its summary.
func (s *Service) ToResponse(a Action) Response { return NewResponse(a, s.Describe(a)) }

// NewResponse renders an action whose summary the caller already has.
func NewResponse(a Action, summary string) Response {
	out := Response{
		ID:              a.ID.String(),
		ToolName:        a.ToolName,
		PermissionLevel: string(a.Permission),
		Status:          a.Status,
		Input:           a.Input,
		Summary:         summary,
		Result:          a.Result,
		ErrorMessage:    a.ErrorMessage,
		CreatedAt:       a.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:       a.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if a.ConversationID != nil {
		id := a.ConversationID.String()
		out.ConversationID = &id
	}
	if out.Result == nil {
		// null rather than absent, so every action has the same keys.
		out.Result = json.RawMessage("null")
	}
	return out
}

// --- handlers ---------------------------------------------------------------

// List handles GET /api/v1/actions.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Status: q.Get("status"), Permission: q.Get("permission_level")}
	if v := q.Get("conversation_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.WriteJSON(w, http.StatusBadRequest, httpx.ErrorBody{
				Error: "validation_failed", Message: "One or more fields are invalid.",
				Fields: httpx.ValidationErrors{{Field: "conversation_id", Message: "must be a uuid"}},
			})
			return
		}
		filter.ConversationID = &id
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

	found, err := h.svc.List(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	effective, _ := ValidateFilter(filter)
	out := listResponse{
		Actions: make([]Response, 0, len(found)),
		Count:   len(found), Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, a := range found {
		out.Actions = append(out.Actions, h.svc.ToResponse(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Get handles GET /api/v1/actions/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	a, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.svc.ToResponse(a))
}

// Approve handles POST /api/v1/actions/{id}/approve: run the proposed write.
//
// It answers 200 with the action in its final state whether the tool
// succeeded or not. The approval was accepted and recorded either way; whether
// the task got created is the action's `status` -- `executed` with a `result`,
// or `failed` with an `error_message` -- and a client reads it there.
func (h *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.svc.Approve)
}

// Reject handles POST /api/v1/actions/{id}/reject. Nothing runs.
func (h *Handler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, h.svc.Reject)
}

// decide is approve and reject: the same scoping, the same empty body, the
// same answers.
//
// An action that exists, is the caller's and is no longer proposed answers
// 409 action_not_pending, naming the state it is in -- the same reasoning as
// the knowledge graph's node_is_backed: nothing about the request is
// malformed, the resource is in a state that forbids it. Somebody else's
// action is 404 whatever its state, because a 409 would confirm the id.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, userID, id uuid.UUID) (Action, error)) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	if !decodeEmpty(w, r) {
		return
	}
	a, err := do(r.Context(), userID, id)
	if errors.Is(err, ErrNotPending) {
		message := "This action is no longer awaiting approval."
		if current, gerr := h.svc.Get(r.Context(), userID, id); gerr == nil {
			message = "This action is no longer awaiting approval: it is " + current.Status + "."
			if current.Permission == tools.Read {
				message = "This action is a read the assistant has already run; only proposed changes are approved or rejected."
			}
		}
		httpx.WriteError(w, http.StatusConflict, "action_not_pending", message)
		return
	}
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.svc.ToResponse(a))
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as an action belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "action")
		return uuid.Nil, uuid.Nil, false
	}
	return userID, id, true
}

// decodeEmpty accepts no body, or a body that is exactly an empty JSON object.
//
// Approve and reject take no parameters, and a body that tries to pass one --
// `{"confirm": true}`, `{"input": {...}}` -- is rejected rather than ignored.
// Ignoring it is how a client comes to believe it edited a proposal on the way
// through, which is precisely what this endpoint must never appear to allow:
// what runs is what was proposed.
func decodeEmpty(w http.ResponseWriter, r *http.Request) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, httpx.MaxBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpx.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large.")
			return false
		}
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "Request body must be valid JSON.")
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt := strings.TrimSpace(strings.Split(ct, ";")[0]); mt != "application/json" {
			httpx.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
			return false
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var empty struct{}
	if err := dec.Decode(&empty); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "This endpoint takes no parameters; send no body or {}.")
		return false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_json", "Request body must contain a single JSON object.")
		return false
	}
	return true
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
		httpx.NotFound(w, "action")
	default:
		h.log.Error("action request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
