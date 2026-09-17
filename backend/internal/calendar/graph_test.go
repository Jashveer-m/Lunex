package calendar

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// --- node sync ---------------------------------------------------------------

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

func newSyncedService() (*Service, *fakeSyncer, uuid.UUID) {
	syncer := &fakeSyncer{}
	return NewService(newFakeStore(), WithNodeSync(syncer)), syncer, uuid.New()
}

func TestCreateSyncsAGraphNode(t *testing.T) {
	svc, syncer, owner := newSyncedService()

	created, err := svc.Create(context.Background(), owner, meeting())
	if err != nil {
		t.Fatal(err)
	}

	seen := syncer.seen()
	if len(seen) != 1 {
		t.Fatalf("creating an event made %d graph calls, want 1", len(seen))
	}
	switch {
	case seen[0].userID != owner:
		t.Fatalf("the node was synced as %v, not the owner", seen[0].userID)
	case seen[0].refTable != graphRefTable:
		t.Fatalf("ref_table = %q, want %q", seen[0].refTable, graphRefTable)
	case seen[0].refID != created.ID:
		t.Fatalf("ref_id = %v, want the event id", seen[0].refID)
	case seen[0].label != "Standup":
		t.Fatalf("label = %q, want the title", seen[0].label)
	}
}

func TestUpdateResyncsTheGraphNode(t *testing.T) {
	svc, syncer, owner := newSyncedService()
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, meeting())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, UpdateInput{Title: optional.Of("Team standup")}); err != nil {
		t.Fatal(err)
	}

	nodes := syncer.nodes()
	if len(nodes) != 1 {
		t.Fatalf("renaming made %d nodes, want 1", len(nodes))
	}
	if got := nodes[created.ID]; got != "Team standup" {
		t.Fatalf("the node label is %q, want the new title", got)
	}
}

// Moving an event without renaming it still syncs -- the node is idempotent,
// and a sync on every write is what makes the update path the repair for a
// create whose sync failed.
func TestMovingAnEventStillSyncs(t *testing.T) {
	svc, syncer, owner := newSyncedService()
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, meeting())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, UpdateInput{
		StartTime: optional.Of(nine.Add(time.Hour)), EndTime: optional.Of(tenAM.Add(time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(syncer.seen()); n != 2 {
		t.Fatalf("create then move made %d graph calls, want 2", n)
	}
	if len(syncer.nodes()) != 1 {
		t.Fatal("moving an event made a second node")
	}
}

func TestARejectedWriteDoesNotSync(t *testing.T) {
	svc, syncer, owner := newSyncedService()
	ctx := context.Background()

	// Each of these fails at a different step: validation, then the ownership
	// check on a link. Neither may reach the graph.
	if _, err := svc.Create(ctx, owner, CreateInput{Title: "   ", StartTime: nine}); err == nil {
		t.Fatal("an empty title was accepted")
	}
	if _, err := svc.Create(ctx, owner, CreateInput{
		Title: "x", StartTime: nine, EndTime: tenAM, RelatedTaskID: idOf(uuid.New()),
	}); err == nil {
		t.Fatal("a link to nothing was accepted")
	}
	if n := len(syncer.seen()); n != 0 {
		t.Fatalf("a rejected create made %d graph calls", n)
	}
}

// Deleting makes no graph call at all: the node goes with the row through the
// trigger, on every path a row can leave by.
func TestDeleteDoesNotCallTheGraph(t *testing.T) {
	svc, syncer, owner := newSyncedService()
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, meeting())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(syncer.seen()); n != 1 {
		t.Fatalf("delete made a graph call: %d calls after one create", n)
	}
}

func TestNoGraphIsANoOp(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.Create(context.Background(), uuid.New(), meeting()); err != nil {
		t.Fatalf("a service with no graph wired failed to create an event: %v", err)
	}
}
