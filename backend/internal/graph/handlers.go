package graph

import (
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/httpx"
)

// Handler adapts Service to HTTP.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Routes returns the subtree mounted at /knowledge-graph. It assumes
// auth.RequireAuth is already in front of it.
//
// There is no POST and no PATCH. Nodes come from the resources they mirror or
// from what a conversation said, and edges come from extraction; a hand-written
// node would have no honest ref, and a hand-written edge no honest confidence.
// The client's powers are the ones the brief names: read the graph, read a
// neighbourhood, and remove what is wrong.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.Graph)
	r.Get("/nodes/{id}", h.Node)
	r.Delete("/nodes/{id}", h.DeleteNode)
	r.Delete("/edges/{id}", h.DeleteEdge)
	return r
}

// --- wire types -------------------------------------------------------------

type nodeResponse struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
	// RefTable and RefID are null for a node a conversation named. Together
	// they are how a client tells the two kinds apart -- and therefore which
	// nodes it may offer a delete button for.
	RefTable *string `json:"ref_table"`
	RefID    *string `json:"ref_id"`
	// Extracted says the same thing as `ref_table == null`, spelled out. It is
	// the field a client actually branches on, and deriving it here means one
	// answer rather than every client writing the same null check.
	Extracted bool   `json:"extracted"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type edgeResponse struct {
	ID           string  `json:"id"`
	FromNodeID   string  `json:"from_node_id"`
	ToNodeID     string  `json:"to_node_id"`
	Relationship string  `json:"relationship"`
	Confidence   float64 `json:"confidence"`
	// SourceConversationID is null once the conversation it came from has been
	// deleted: the edge outlives its source, and says so.
	SourceConversationID *string `json:"source_conversation_id"`
	CreatedAt            string  `json:"created_at"`
}

type graphResponse struct {
	Nodes     []nodeResponse `json:"nodes"`
	Edges     []edgeResponse `json:"edges"`
	NodeCount int            `json:"node_count"`
	EdgeCount int            `json:"edge_count"`
	Limit     int            `json:"limit"`
	Offset    int            `json:"offset"`
}

// neighborResponse is one step out from a node. The edge is nested rather than
// flattened so a client can act on it -- DELETE /edges/{id} needs the edge id,
// and confidence belongs to the edge, not to the node on the far side.
type neighborResponse struct {
	Node     nodeResponse `json:"node"`
	Edge     edgeResponse `json:"edge"`
	Incoming bool         `json:"incoming"`
}

type neighborhoodResponse struct {
	Node          nodeResponse       `json:"node"`
	Neighbors     []neighborResponse `json:"neighbors"`
	NeighborCount int                `json:"neighbor_count"`
}

func toNodeResponse(n Node) nodeResponse {
	out := nodeResponse{
		ID:        n.ID.String(),
		Type:      n.Type,
		Label:     n.Label,
		RefTable:  n.RefTable,
		Extracted: n.Extracted(),
		CreatedAt: n.CreatedAt.UTC().Format(httpx.TimeFormat),
		UpdatedAt: n.UpdatedAt.UTC().Format(httpx.TimeFormat),
	}
	if n.RefID != nil {
		id := n.RefID.String()
		out.RefID = &id
	}
	return out
}

func toEdgeResponse(e Edge) edgeResponse {
	out := edgeResponse{
		ID:           e.ID.String(),
		FromNodeID:   e.FromNodeID.String(),
		ToNodeID:     e.ToNodeID.String(),
		Relationship: e.Relationship,
		// Rounded because the column is `real`: single precision turns 0.7
		// into 0.699999988079071 on the way back, and a score the model gave
		// to one decimal place should not be reported to fifteen.
		Confidence: round2(e.Confidence),
		CreatedAt:  e.CreatedAt.UTC().Format(httpx.TimeFormat),
	}
	if e.SourceConversationID != nil {
		id := e.SourceConversationID.String()
		out.SourceConversationID = &id
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// --- handlers ---------------------------------------------------------------

// Graph handles GET /api/v1/knowledge-graph.
func (h *Handler) Graph(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}

	q := r.URL.Query()
	filter := Filter{Type: q.Get("type")}
	var err error
	if filter.Limit, err = intParam(q.Get("limit")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "limit must be a whole number.")
		return
	}
	if filter.Offset, err = intParam(q.Get("offset")); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "validation_failed", "offset must be a whole number.")
		return
	}

	g, err := h.svc.Graph(r.Context(), userID, filter)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	effective, _ := ValidateFilter(filter)
	out := graphResponse{
		Nodes:     make([]nodeResponse, 0, len(g.Nodes)),
		Edges:     make([]edgeResponse, 0, len(g.Edges)),
		NodeCount: len(g.Nodes), EdgeCount: len(g.Edges),
		Limit: effective.Limit, Offset: effective.Offset,
	}
	for _, n := range g.Nodes {
		out.Nodes = append(out.Nodes, toNodeResponse(n))
	}
	for _, e := range g.Edges {
		out.Edges = append(out.Edges, toEdgeResponse(e))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Node handles GET /api/v1/knowledge-graph/nodes/{id}: one node and what it is
// connected to.
func (h *Handler) Node(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	n, err := h.svc.Node(r.Context(), userID, id)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := neighborhoodResponse{
		Node:          toNodeResponse(n.Node),
		Neighbors:     make([]neighborResponse, 0, len(n.Neighbors)),
		NeighborCount: len(n.Neighbors),
	}
	for _, nb := range n.Neighbors {
		out.Neighbors = append(out.Neighbors, neighborResponse{
			Node: toNodeResponse(nb.Node), Edge: toEdgeResponse(nb.Edge), Incoming: nb.Incoming,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// DeleteNode handles DELETE /api/v1/knowledge-graph/nodes/{id}.
//
// A node backed by a task, goal, note or document answers 409 with a message
// naming what to delete instead. It is not a 404 -- the node exists and the
// caller owns it -- and not a 400, because nothing about the request is
// malformed: the resource is in a state that forbids the operation, which is
// what 409 means.
func (h *Handler) DeleteNode(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteNode(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteEdge handles DELETE /api/v1/knowledge-graph/edges/{id}.
func (h *Handler) DeleteEdge(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.scope(w, r)
	if !ok {
		return
	}
	if err := h.svc.DeleteEdge(r.Context(), userID, id); err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ----------------------------------------------------------------

// scope pulls the authenticated user and the path id. A malformed id answers
// 404, the same as a node or edge belonging to somebody else.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.NotFound(w, "graph node or edge")
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
	case errors.Is(err, ErrNodeIsBacked):
		httpx.WriteError(w, http.StatusConflict, "node_is_backed",
			"This node mirrors a task, goal, note or document and follows it. "+
				"Delete that record instead and the node goes with it.")
	case errors.Is(err, ErrNotFound):
		httpx.NotFound(w, "graph node or edge")
	default:
		h.log.Error("knowledge graph request failed",
			"error", err, "path", r.URL.Path, "method", r.Method)
		httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "Something went wrong.")
	}
}
