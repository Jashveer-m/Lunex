package goals

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// --- Phase 6: node sync -----------------------------------------------------

// fakeSyncer records every call the write path makes into the knowledge graph,
// and collapses them the way the upsert does, so a test can see "one node per
// row" rather than only "SyncNode was called".
type fakeSyncer struct {
	mu    sync.Mutex
	calls []syncCall
}

type syncCall struct {
	userID   uuid.UUID
	refTable string
	refID    uuid.UUID
	label    string
}

func (f *fakeSyncer) SyncNode(_ context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, syncCall{userID: userID, refTable: refTable, refID: refID, label: label})
}

func (f *fakeSyncer) seen() []syncCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncCall(nil), f.calls...)
}

// nodes is what the graph would hold after these calls: one entry per
// (ref_table, ref_id), carrying the label from the most recent sync.
func (f *fakeSyncer) nodes() map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, c := range f.seen() {
		out[c.refID] = c.label
	}
	return out
}

var _ NodeSyncer = (*fakeSyncer)(nil)

func newSyncedService(t *testing.T) (*Service, *fakeSyncer, uuid.UUID) {
	t.Helper()
	syncer := &fakeSyncer{}
	return NewService(newFakeStore(), WithNodeSync(syncer)), syncer, uuid.New()
}

// The property sync-on-write exists for: creating a goal puts a node in the
// graph, keyed to the row and labelled with its title. It is the node an
// extracted relationship then attaches to when a conversation names the goal.
func TestCreateSyncsAGraphNode(t *testing.T) {
	svc, syncer, owner := newSyncedService(t)

	created, err := svc.Create(context.Background(), owner, CreateInput{Title: "the backend project", Type: "project"})
	if err != nil {
		t.Fatal(err)
	}

	seen := syncer.seen()
	if len(seen) != 1 {
		t.Fatalf("creating a goal made %d graph calls, want 1", len(seen))
	}
	switch {
	case seen[0].userID != owner:
		t.Fatalf("the node was synced as %v, not the owner", seen[0].userID)
	case seen[0].refTable != graphRefTable:
		t.Fatalf("ref_table = %q, want %q", seen[0].refTable, graphRefTable)
	case seen[0].refID != created.ID:
		t.Fatalf("ref_id = %v, want the goal id", seen[0].refID)
	case seen[0].label != "the backend project":
		t.Fatalf("label = %q, want the title", seen[0].label)
	}
}

func TestUpdateResyncsTheGraphNode(t *testing.T) {
	svc, syncer, owner := newSyncedService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, CreateInput{Title: "the backend project", Type: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, UpdateInput{Title: optional.Of("the API rewrite")}); err != nil {
		t.Fatal(err)
	}

	nodes := syncer.nodes()
	if len(nodes) != 1 {
		t.Fatalf("renaming made %d nodes, want 1", len(nodes))
	}
	if got := nodes[created.ID]; got != "the API rewrite" {
		t.Fatalf("the node label is %q, want the new title", got)
	}
}

func TestARejectedWriteDoesNotSync(t *testing.T) {
	svc, syncer, owner := newSyncedService(t)
	// An unknown goal type is a validation failure, so no row is written.
	if _, err := svc.Create(context.Background(), owner, CreateInput{Title: "g", Type: "nonsense"}); err == nil {
		t.Fatal("an unknown goal type was accepted")
	}
	if n := len(syncer.seen()); n != 0 {
		t.Fatalf("a rejected create made %d graph calls", n)
	}
}

func TestNoGraphIsANoOp(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.Create(context.Background(), uuid.New(), CreateInput{Title: "g", Type: "personal"}); err != nil {
		t.Fatalf("a service with no graph wired failed to create a goal: %v", err)
	}
}
