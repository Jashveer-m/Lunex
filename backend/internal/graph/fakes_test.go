package graph

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The fakes key everything by owner, the same shape the SQL has, so a service
// that forgot to pass the caller's id shows up here as a miss rather than as
// data belonging to nobody. They also enforce the two unique indexes from
// migration 000006 in memory, because "Go mentioned twice is one node" is a
// property this package's tests have to be able to see without Postgres.

type fakeStore struct {
	mu    sync.Mutex
	nodes map[uuid.UUID][]Node
	edges map[uuid.UUID][]Edge
	// callers records the user id every method was given.
	callers []uuid.UUID
	// createEdgeErr fails the write, after extraction and resolution succeeded.
	createEdgeErr error
	nodesErr      error
	clock         time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes: map[uuid.UUID][]Node{},
		edges: map[uuid.UUID][]Edge{},
		clock: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeStore) tick() time.Time {
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

func (f *fakeStore) EnsureRefNode(_ context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, nodeType, label string) (Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	// The (user_id, ref_table, ref_id) unique index.
	for i, n := range f.nodes[userID] {
		if n.RefTable != nil && *n.RefTable == refTable && n.RefID != nil && *n.RefID == refID {
			f.nodes[userID][i].Label = label
			f.nodes[userID][i].UpdatedAt = f.tick()
			return f.nodes[userID][i], nil
		}
	}
	at := f.tick()
	n := Node{
		ID: uuid.New(), UserID: userID, Type: nodeType, Label: label,
		RefTable: &refTable, RefID: &refID, CreatedAt: at, UpdatedAt: at,
	}
	f.nodes[userID] = append(f.nodes[userID], n)
	return n, nil
}

func (f *fakeStore) EnsureExtractedNode(_ context.Context, userID uuid.UUID, nodeType, label string) (Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	// The (user_id, type, lower(label)) partial unique index.
	for i, n := range f.nodes[userID] {
		if n.Extracted() && n.Type == nodeType && strings.EqualFold(n.Label, label) {
			f.nodes[userID][i].Label = label
			return f.nodes[userID][i], nil
		}
	}
	at := f.tick()
	n := Node{ID: uuid.New(), UserID: userID, Type: nodeType, Label: label, CreatedAt: at, UpdatedAt: at}
	f.nodes[userID] = append(f.nodes[userID], n)
	return n, nil
}

func (f *fakeStore) NodesByLabel(_ context.Context, userID uuid.UUID, label string) ([]Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	out := []Node{}
	for _, n := range f.nodes[userID] {
		if strings.EqualFold(n.Label, label) {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeStore) NodeByID(_ context.Context, userID, id uuid.UUID) (Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for _, n := range f.nodes[userID] {
		if n.ID == id {
			return n, nil
		}
	}
	return Node{}, ErrNotFound
}

func (f *fakeStore) Nodes(_ context.Context, userID uuid.UUID, filter Filter) ([]Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.nodesErr != nil {
		return nil, f.nodesErr
	}
	out := []Node{}
	for _, n := range f.nodes[userID] {
		if filter.Type != "" && n.Type != filter.Type {
			continue
		}
		out = append(out, n)
	}
	if filter.Offset < len(out) {
		out = out[filter.Offset:]
	} else {
		out = []Node{}
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// MentionCandidates is the SQL prefilter: a plain substring match, deliberately
// over-collecting, so the word-boundary test in the service is the thing under
// test rather than a fake that already did its job.
func (f *fakeStore) MentionCandidates(_ context.Context, userID uuid.UUID, text string, limit int) ([]Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	out := []Node{}
	for _, n := range f.nodes[userID] {
		if len(n.Label) < MinMentionLen {
			continue
		}
		if strings.Contains(strings.ToLower(text), strings.ToLower(n.Label)) {
			out = append(out, n)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteNode(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for i, n := range f.nodes[userID] {
		if n.ID == id {
			f.nodes[userID] = append(f.nodes[userID][:i], f.nodes[userID][i+1:]...)
			// The FK cascade from knowledge_nodes to knowledge_edges.
			kept := []Edge{}
			for _, e := range f.edges[userID] {
				if e.FromNodeID != id && e.ToNodeID != id {
					kept = append(kept, e)
				}
			}
			f.edges[userID] = kept
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) CreateEdge(_ context.Context, userID uuid.UUID, in EdgeInput) (Edge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.createEdgeErr != nil {
		return Edge{}, f.createEdgeErr
	}
	// The (user_id, from, to, relationship) unique index, with the same
	// GREATEST-confidence upsert the repository does.
	for i, e := range f.edges[userID] {
		if e.FromNodeID == in.FromNodeID && e.ToNodeID == in.ToNodeID && e.Relationship == in.Relationship {
			if in.Confidence > e.Confidence {
				f.edges[userID][i].Confidence = in.Confidence
			}
			return f.edges[userID][i], nil
		}
	}
	e := Edge{
		ID: uuid.New(), UserID: userID, FromNodeID: in.FromNodeID, ToNodeID: in.ToNodeID,
		Relationship: in.Relationship, Confidence: in.Confidence,
		SourceConversationID: in.SourceConversationID, CreatedAt: f.tick(),
	}
	f.edges[userID] = append(f.edges[userID], e)
	return e, nil
}

func (f *fakeStore) EdgesAmong(_ context.Context, userID uuid.UUID, nodeIDs []uuid.UUID, limit int) ([]Edge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	in := make(map[uuid.UUID]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		in[id] = struct{}{}
	}
	out := []Edge{}
	for _, e := range f.edges[userID] {
		if _, ok := in[e.FromNodeID]; !ok {
			continue
		}
		if _, ok := in[e.ToNodeID]; !ok {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) Neighbors(_ context.Context, userID, nodeID uuid.UUID, limit int) ([]Neighbor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	byID := map[uuid.UUID]Node{}
	for _, n := range f.nodes[userID] {
		byID[n.ID] = n
	}
	out := []Neighbor{}
	for _, e := range f.edges[userID] {
		switch nodeID {
		case e.FromNodeID:
			out = append(out, Neighbor{Edge: e, Node: byID[e.ToNodeID]})
		case e.ToNodeID:
			out = append(out, Neighbor{Edge: e, Node: byID[e.FromNodeID], Incoming: true})
		default:
			continue
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteEdge(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for i, e := range f.edges[userID] {
		if e.ID == id {
			f.edges[userID] = append(f.edges[userID][:i], f.edges[userID][i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// --- inspection -------------------------------------------------------------

func (f *fakeStore) nodeCount(userID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.nodes[userID])
}

func (f *fakeStore) edgeCount(userID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.edges[userID])
}

func (f *fakeStore) labels(userID uuid.UUID) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.nodes[userID]))
	for _, n := range f.nodes[userID] {
		out = append(out, n.Label)
	}
	return out
}

func (f *fakeStore) callersSeen() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.callers...)
}

var _ Store = (*fakeStore)(nil)
