package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/httpx"
	"github.com/jashveer/lifeos/backend/internal/memories"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Routes returns the subtree mounted at /conversations. It assumes
// auth.RequireAuth is already in front of it.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{id}", h.Get)
	r.Delete("/{id}", h.Delete)
	r.Post("/{id}/messages", h.SendMessage)
	return r
}

// --- wire types -------------------------------------------------------------

type createRequest struct {
	Title string `json:"title"`
}

type messageRequest struct {
	Content string `json:"content"`
}

type conversationResponse struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	// Messages is present on the single-conversation read only.
	Messages []messageResponse `json:"messages,omitempty"`
}

type messageResponse struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// Sources is [] rather than null on the wire so a client needs no special
	// case; the column still distinguishes the two.
	Sources   []Source `json:"sources"`
	CreatedAt string   `json:"created_at"`
}

type listResponse struct {
	Conversations []conversationResponse `json:"conversations"`
	Count         int                    `json:"count"`
	Limit         int                    `json:"limit"`
	Offset        int                    `json:"offset"`
}

// sourcesEvent is the first frame of a stream: everything retrieved, before a
// single token is generated, so a client can show what the answer will be
// grounded in while it is still being written.
type sourcesEvent struct {
	Sources []Source `json:"sources"`
	Count   int      `json:"count"`
}

type tokenEvent struct {
	// JSON-encoded rather than written raw: SSE frames are newline-delimited,
	// and a model emitting a newline mid-sentence would otherwise end the
	// frame.
	Text string `json:"text"`
}

type doneEvent struct {
	ConversationID string          `json:"conversation_id"`
	UserMessage    messageResponse `json:"user_message"`
	Message        messageResponse `json:"message"`
	Model          string          `json:"model"`
	// Remembered is what the assistant learned from this exchange, so a client
	// can show it. It is [] on the great majority of turns. The full record,
	// with the scores and the switches, is at /api/v1/memories -- this is the
	// notification, not the management surface.
	Remembered []rememberedResponse `json:"remembered"`
	// Linked is what the turn added to the knowledge graph, on the same terms:
	// [] on most turns, the notification rather than the management surface.
	// The full graph is at /api/v1/knowledge-graph.
	Linked []linkedResponse `json:"linked"`
	// Actions is what the turn did with a tool -- a search it ran, or a change
	// it proposed that now waits for approval at /api/v1/actions/{id}/approve.
	// The same actions were already sent as `action` frames the moment the
	// turn was saved; they are repeated here so a client reading only `done`
	// misses nothing. [] on most turns.
	Actions []actions.Response `json:"actions"`
}

func toActionResponses(acts []TurnAction) []actions.Response {
	out := make([]actions.Response, 0, len(acts))
	for _, a := range acts {
		out = append(out, actions.NewResponse(a.Action, a.Summary))
	}
	return out
}

// linkedResponse is one newly recorded relationship, reported on the turn that
// produced it. The node ids are what a client follows to
// /knowledge-graph/nodes/{id}; the labels are what it can show without a
// second request.
type linkedResponse struct {
	ID           string  `json:"id"`
	FromNodeID   string  `json:"from_node_id"`
	ToNodeID     string  `json:"to_node_id"`
	Relationship string  `json:"relationship"`
	Confidence   float64 `json:"confidence"`
}

// rememberedResponse is one newly stored memory, reported on the turn that
// produced it.
type rememberedResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

func toRememberedResponse(stored []memories.Memory) []rememberedResponse {
	out := make([]rememberedResponse, 0, len(stored))
	for _, m := range stored {
		out = append(out, rememberedResponse{ID: m.ID.String(), Type: m.Type, Content: m.Content})
	}
	return out
}

func toLinkedResponse(stored []graph.Edge) []linkedResponse {
	out := make([]linkedResponse, 0, len(stored))
	for _, e := range stored {
		out = append(out, linkedResponse{
			ID: e.ID.String(), FromNodeID: e.FromNodeID.String(), ToNodeID: e.ToNodeID.String(),
			Relationship: e.Relationship,
			// Rounded because the column is `real`; see graph's own handler.
			Confidence: math.Round(e.Confidence*100) / 100,
		})
	}
	return out
}

func toResponse(c Conversation, withMessages bool) conversationResponse {
	out := conversationResponse{
		ID:           c.ID.String(),
		Title:        c.Title,
		MessageCount: c.MessageCount,
		CreatedAt:    c.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt:    c.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if withMessages {
		out.Messages = make([]messageResponse, 0, len(c.Messages))
		for _, m := range c.Messages {
			out.Messages = append(out.Messages, toMessageResponse(m))
		}
	}
	return out
}

func toMessageResponse(m Message) messageResponse {
	sources := m.Sources
	if sources == nil {
		sources = []Source{}
	}
	return messageResponse{
		ID:        m.ID.String(),
		Role:      m.Role,
		Content:   m.Content,
		Sources:   sources,
		CreatedAt: m.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
}

// --- handlers ---------------------------------------------------------------

// Create handles POST /api/v1/conversations.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	var req createRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	conv, err := h.svc.Create(r.Context(), userID, req.Title)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toResponse(conv, false))
}

// List handles GET /api/v1/conversations.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Sort: q.Get("sort")}
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
		Conversations: make([]conversationResponse, 0, len(found)),
		Count:         len(found), Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, c := range found {
		out.Conversations = append(out.Conversations, toResponse(c, false))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Get handles GET /api/v1/conversations/{id}: the conversation with its
// messages and their recorded sources.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	conv, err := h.svc.Get(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toResponse(conv, true))
}

// Delete handles DELETE /api/v1/conversations/{id}. Messages go with it, by
// cascade.
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

// SendMessage handles POST /api/v1/conversations/{id}/messages and streams the
// answer as Server-Sent Events.
//
// The response is a normal JSON error until the first event is written, and
// SSE afterwards. That split is the whole reason the orchestrator retrieves
// and opens the model stream before touching the sink: an unknown
// conversation, an invalid body, or an unreachable model all still get a real
// status code, and only a failure during generation has to be reported as an
// `error` event on an already-committed 200.
func (h *Handler) SendMessage(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req messageRequest
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}

	sink := newSSESink(w)
	turn, err := h.svc.SendMessage(r.Context(), userID, id, req.Content, sink)
	if err != nil {
		if !sink.started {
			h.writeServiceError(w, r, err)
			return
		}
		// The client hanging up is not a failure to report to the client.
		if errors.Is(err, errClientGone) || r.Context().Err() != nil {
			h.log.Info("chat stream abandoned", "user_id", userID, "conversation_id", id, "error", err)
			return
		}
		h.log.Error("chat generation failed", "error", err, "user_id", userID, "conversation_id", id)
		code, message := "internal_error", "The answer could not be completed, and nothing was saved. Send the message again."
		if Unavailable(err) {
			code = "model_unavailable"
		}
		_ = sink.send("error", httpx.ErrorBody{Error: code, Message: message})
		return
	}

	_ = sink.send("done", doneEvent{
		ConversationID: id.String(),
		UserMessage:    toMessageResponse(turn.User),
		Message:        toMessageResponse(turn.Assistant),
		Model:          turn.Model,
		Remembered:     toRememberedResponse(turn.Remembered),
		Linked:         toLinkedResponse(turn.Linked),
		Actions:        toActionResponses(turn.Actions),
	})
}

// --- SSE --------------------------------------------------------------------

// errClientGone marks a write that failed because nobody is listening.
var errClientGone = errors.New("client disconnected")

// sseSink writes the turn as Server-Sent Events.
//
// It writes no headers until the first event, which is what lets the handler
// above still answer with a status code for everything that fails early.
type sseSink struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func newSSESink(w http.ResponseWriter) *sseSink {
	return &sseSink{w: w, rc: http.NewResponseController(w)}
}

func (s *sseSink) Sources(sources []Source) error {
	if sources == nil {
		sources = []Source{}
	}
	return s.send("sources", sourcesEvent{Sources: sources, Count: len(sources)})
}

func (s *sseSink) Token(text string) error {
	return s.send("token", tokenEvent{Text: text})
}

// Actions sends one `action` frame per action, each an action exactly as
// GET /api/v1/actions/{id} returns it. They follow the last token and precede
// `done`: the row exists by the time the frame is written, so a client can
// approve the moment it shows the proposal.
func (s *sseSink) Actions(acts []TurnAction) error {
	for _, a := range acts {
		if err := s.send("action", actions.NewResponse(a.Action, a.Summary)); err != nil {
			return err
		}
	}
	return nil
}

func (s *sseSink) send(event string, payload any) error {
	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		// Cache-Control is already no-store from the security middleware.
		h.Set("Connection", "keep-alive")
		// Tells nginx not to buffer, which would hold the whole answer back
		// and defeat the point of streaming it.
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", event, err)
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("%w: %v", errClientGone, err)
	}
	// Without the flush the answer arrives in one lump when the handler
	// returns, which is the thing streaming exists to avoid.
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("%w: flush: %v", errClientGone, err)
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a conversation belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "conversation")
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
		httpx.NotFound(w, "conversation")
	case Unavailable(err):
		// The assistant cannot answer without the model, and retrieval cannot
		// run without the embedding service. Both are outages, not bad
		// requests, and 503 is the code a client should retry on.
		h.log.Error("chat unavailable", "error", err, "path", r.URL.Path)
		httpx.WriteError(w, http.StatusServiceUnavailable, "model_unavailable",
			"The assistant is temporarily unavailable because a model service could not be reached.")
	default:
		h.log.Error("chat request failed", "error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
