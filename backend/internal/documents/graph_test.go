package documents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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

var _ NodeSyncer = (*fakeSyncer)(nil)

func newSyncedService(t *testing.T) (*Service, *fakeStore, *fakeSyncer) {
	t.Helper()
	store, syncer := newFakeStore(), &fakeSyncer{}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(store, newFakeEmbedder(), discard, 5*time.Second, WithNodeSync(syncer)), store, syncer
}

// The property sync-on-write exists for: uploading a document puts a node in
// the graph, keyed to the row and labelled with its filename.
func TestUploadSyncsAGraphNode(t *testing.T) {
	svc, _, syncer := newSyncedService(t)

	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "field-notes.txt", FileType: TypeText,
		Content: []byte("Aurora borealis over the tundra."),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusReady {
		t.Fatalf("status = %q, want ready", doc.Status)
	}

	seen := syncer.seen()
	if len(seen) != 1 {
		t.Fatalf("uploading made %d graph calls, want 1", len(seen))
	}
	switch {
	case seen[0].userID != doc.UserID:
		t.Fatalf("the node was synced as %v, not the owner", seen[0].userID)
	case seen[0].refTable != graphRefTable:
		t.Fatalf("ref_table = %q, want %q", seen[0].refTable, graphRefTable)
	case seen[0].refID != doc.ID:
		t.Fatalf("ref_id = %v, want the document id", seen[0].refID)
	case seen[0].label != "field-notes.txt":
		t.Fatalf("label = %q, want the filename", seen[0].label)
	}
}

// A document whose processing failed is still synced. The node stands for the
// document, not for its text: the row is listable, readable and deletable, its
// filename is a name a later conversation may well use, and there is no update
// path to mirror it later because a document cannot be renamed.
func TestAFailedUploadIsStillSynced(t *testing.T) {
	svc, store, syncer := newSyncedService(t)
	store.completeErr = errors.New("pq: the write failed")

	doc, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "broken.txt", FileType: TypeText, Content: []byte("some text to chunk"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", doc.Status)
	}
	seen := syncer.seen()
	if len(seen) != 1 || seen[0].refID != doc.ID {
		t.Fatalf("a failed document made %d graph calls: %+v", len(seen), seen)
	}
}

// A service built without the option behaves exactly as it did in Phase 3.
func TestNoGraphIsANoOp(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.Upload(context.Background(), uuid.New(), UploadInput{
		Filename: "notes.txt", FileType: TypeText, Content: []byte("some text"),
	}); err != nil {
		t.Fatalf("a service with no graph wired failed to upload: %v", err)
	}
}
