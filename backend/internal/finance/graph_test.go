package finance

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

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

func newSyncedService() (*Service, *fakeStore, *fakeSyncer, uuid.UUID) {
	store, syncer := newFakeStore(), &fakeSyncer{}
	return NewService(store, WithNodeSync(syncer)), store, syncer, uuid.New()
}

func TestCreateSyncsAGraphNode(t *testing.T) {
	svc, _, syncer, owner := newSyncedService()

	created, err := svc.Create(context.Background(), owner, lunch())
	if err != nil {
		t.Fatal(err)
	}

	seen := syncer.seen()
	if len(seen) != 1 {
		t.Fatalf("creating an expense made %d graph calls, want 1", len(seen))
	}
	switch {
	case seen[0].userID != owner:
		t.Fatalf("the node was synced as %v, not the owner", seen[0].userID)
	case seen[0].refTable != graphRefTable:
		t.Fatalf("ref_table = %q, want %q", seen[0].refTable, graphRefTable)
	case seen[0].refID != created.ID:
		t.Fatalf("ref_id = %v, want the expense id", seen[0].refID)
	case seen[0].label != "lunch with the team":
		t.Fatalf("label = %q, want the description", seen[0].label)
	}
}

// An expense has no title, so its label is built -- and it is never the bare
// word "expense". The graph's mention scan matches a node whose label occurs in
// the message, so a node called "Expense" would fire on every question
// containing the word.
func TestANodeLabelIsNeverJustTheWordExpense(t *testing.T) {
	svc, store, syncer, owner := newSyncedService()
	ctx := context.Background()
	cats := store.seedDefaults(owner)
	food := cats["Food"]

	if _, err := svc.Create(ctx, owner, CreateInput{Amount: 100, Date: sept17, CategoryID: &food}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, owner, CreateInput{Amount: 100, Date: sept17}); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"Food expense on 2026-09-17": true,
		"Expense on 2026-09-17":      true,
	}
	for id, label := range syncer.nodes() {
		if !want[label] {
			t.Fatalf("node %v is labelled %q, want one of %v", id, label, want)
		}
		if strings.EqualFold(strings.TrimSpace(label), "expense") {
			t.Fatalf("node %v is labelled just %q", id, label)
		}
	}
}

func TestUpdateResyncsTheGraphNode(t *testing.T) {
	svc, _, syncer, owner := newSyncedService()
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, lunch())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(ctx, owner, created.ID, UpdateInput{
		Description: optional.Of("lunch with the design team"),
	}); err != nil {
		t.Fatal(err)
	}

	nodes := syncer.nodes()
	if len(nodes) != 1 {
		t.Fatalf("re-describing made %d nodes, want 1", len(nodes))
	}
	if got := nodes[created.ID]; got != "lunch with the design team" {
		t.Fatalf("the node label is %q, want the new description", got)
	}
}

func TestARejectedWriteDoesNotSync(t *testing.T) {
	svc, _, syncer, owner := newSyncedService()
	ctx := context.Background()

	// Each of these fails at a different step: validation, then the ownership
	// check on a link. Neither may reach the graph.
	if _, err := svc.Create(ctx, owner, CreateInput{Amount: 0, Date: sept17}); err == nil {
		t.Fatal("an expense of nothing was accepted")
	}
	stranger := uuid.New()
	if _, err := svc.Create(ctx, owner, CreateInput{
		Amount: 100, Date: sept17, CategoryID: &stranger,
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
	svc, _, syncer, owner := newSyncedService()
	ctx := context.Background()

	created, err := svc.Create(ctx, owner, lunch())
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

// Categories are not mirrored: five nodes per account that the mention scan
// would match on the word "food" is worse than none.
func TestCategoriesGetNoGraphNode(t *testing.T) {
	svc, _, syncer, owner := newSyncedService()

	if _, err := svc.CreateCategory(context.Background(), owner, "Books"); err != nil {
		t.Fatal(err)
	}
	if n := len(syncer.seen()); n != 0 {
		t.Fatalf("creating a category made %d graph calls", n)
	}
}

func TestNoGraphIsANoOp(t *testing.T) {
	svc := NewService(newFakeStore())
	if _, err := svc.Create(context.Background(), uuid.New(), lunch()); err != nil {
		t.Fatalf("a service with no graph wired failed to create an expense: %v", err)
	}
}
