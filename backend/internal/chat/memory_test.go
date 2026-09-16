package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// memoryHarness is the Phase 4 harness with both halves of the memory system
// wired in.
type memoryHarness struct {
	*harness
	mem       *fakeMemories
	extractor *fakeExtractor
}

func newMemoryHarness(t *testing.T, provider *ai.Mock) *memoryHarness {
	t.Helper()
	if provider == nil {
		provider = &ai.Mock{}
	}
	base := &harness{
		store:    newFakeStore(),
		docs:     &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		tasks:    &fakeTasks{byUser: map[uuid.UUID][]tasks.Task{}},
		goals:    &fakeGoals{byUser: map[uuid.UUID][]goals.Goal{}},
		notes:    &fakeNotes{byUser: map[uuid.UUID][]notes.Note{}},
		provider: provider,
		user:     uuid.New(),
	}
	h := &memoryHarness{
		harness:   base,
		mem:       &fakeMemories{byUser: map[uuid.UUID][]memories.SearchResult{}},
		extractor: &fakeExtractor{},
	}
	base.svc = NewService(Deps{
		Store: base.store, Provider: provider,
		Documents: base.docs, Memories: h.mem, MemoryExtractor: h.extractor,
		Tasks: base.tasks, Goals: base.goals, Notes: base.notes,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	base.svc.now = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	base.conv = base.store.seed(base.user)
	return h
}

func (h *memoryHarness) withMemory(owner uuid.UUID, kind, content string, similarity float64) memories.SearchResult {
	r := memories.SearchResult{
		Memory: memories.Memory{
			ID: uuid.New(), UserID: owner, Type: kind, Content: content,
			Importance: 0.8, Confidence: 0.9, Enabled: true,
		},
		Similarity: similarity,
	}
	h.mem.byUser[owner] = append(h.mem.byUser[owner], r)
	return r
}

// --- retrieval --------------------------------------------------------------

// The property Phase 5 exists for: something the assistant learned in an
// earlier conversation reaches the prompt of a later one, is offered as a
// citable source, and is recorded as cited when the answer uses it.
func TestARememberedFactGroundsALaterAnswer(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.Contains(ai.PromptText(msgs), "prefers studying in the morning") {
			return "Mornings, going by what you told me before [S1]."
		}
		return "I could not find anything about that."
	}})
	fact := h.withMemory(h.user, memories.TypePreference,
		"The user prefers studying in the morning before class.", 0.74)

	turn, sink, err := h.send(t, "when should I schedule my revision?")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// The memory reached the model, labelled -- and without its score, which
	// the model would otherwise repeat to the user. The score is on the source.
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{"prefers studying in the morning", "[S1] memory:"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "similarity 0.74") {
		t.Fatalf("prompt leaks the retrieval score:\n%s", prompt)
	}

	if len(sink.Retrieved) != 1 {
		t.Fatalf("sink saw %d sources, want 1", len(sink.Retrieved))
	}
	got := turn.Assistant.Sources[0]
	switch {
	case got.Type != SourceMemory:
		t.Fatalf("type = %q, want %q", got.Type, SourceMemory)
	case got.ID != fact.ID:
		t.Fatalf("id = %v, want the memory it came from (%v)", got.ID, fact.ID)
	case got.Title != memories.TypePreference:
		t.Fatalf("title = %q, want the memory's type", got.Title)
	case got.Excerpt != fact.Content:
		t.Fatalf("excerpt = %q, want the fact itself", got.Excerpt)
	case got.Similarity == nil || *got.Similarity != 0.74:
		t.Fatalf("similarity = %v, want the retrieval score", got.Similarity)
	case got.ChunkIndex != nil:
		t.Fatalf("chunk_index = %v, want nil -- a memory has no chunks", *got.ChunkIndex)
	case !got.Cited:
		t.Fatal("the answer cited [S1] but the source is recorded as uncited")
	}
}

// Memories are retrieved with their own floor, not the document one, and it is
// applied rather than merely passed along.
func TestMemoryRetrievalUsesItsOwnFloor(t *testing.T) {
	h := newMemoryHarness(t, nil)
	h.svc.opts.MinSimilarity = 0.5
	h.svc.opts.MemoryMinSimilarity = 0.6
	h.withMemory(h.user, memories.TypeSemantic, "The user knows Go.", 0.55)

	turn, _, err := h.send(t, "what languages do I know?")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Assistant.Sources) != 0 {
		t.Fatalf("a memory below the memory floor was retrieved: %+v", turn.Assistant.Sources)
	}
	if len(h.mem.queries) != 1 {
		t.Fatalf("the memory store was searched %d times, want once", len(h.mem.queries))
	}
	q := h.mem.queries[0]
	if q.MinSimilarity != 0.6 {
		t.Fatalf("searched with floor %v, want the memory floor 0.6 rather than the document one", q.MinSimilarity)
	}
	if q.Limit != MaxMemories {
		t.Fatalf("top-k = %d, want %d", q.Limit, MaxMemories)
	}
}

// The floor defaults to the memory package's own number, which is higher than
// the document one for the reasons documented there.
func TestTheMemoryFloorDefaultsHigherThanTheDocumentFloor(t *testing.T) {
	h := newMemoryHarness(t, nil)
	if h.svc.opts.MemoryMinSimilarity != memories.DefaultMinSimilarity {
		t.Fatalf("memory floor = %v, want %v", h.svc.opts.MemoryMinSimilarity, memories.DefaultMinSimilarity)
	}
	if h.svc.opts.MemoryMinSimilarity <= h.svc.opts.MinSimilarity {
		t.Fatalf("the memory floor (%v) is not above the document floor (%v)",
			h.svc.opts.MemoryMinSimilarity, h.svc.opts.MinSimilarity)
	}
}

// Another user's memories are unreachable, for the same structural reason
// their documents are: the search is made with the caller's id.
func TestMemoryRetrievalCannotCrossUsers(t *testing.T) {
	h := newMemoryHarness(t, nil)
	stranger := uuid.New()
	h.withMemory(stranger, memories.TypeSemantic, "The stranger's launch code is quetzal seventeen.", 0.99)

	turn, _, err := h.send(t, "what is my launch code?")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Assistant.Sources) != 0 {
		t.Fatalf("retrieved another user's memory: %+v", turn.Assistant.Sources)
	}
	for i, caller := range h.mem.callers {
		if caller != h.user {
			t.Fatalf("memory search %d was made for %v, want the caller %v", i, caller, h.user)
		}
	}
}

// Retrieval order decides the labels and which source is dropped first when the
// budget runs out. Documents lead, memories follow, then the heuristic items.
func TestMemoriesAreLabelledAfterDocumentsAndBeforeItems(t *testing.T) {
	h := newMemoryHarness(t, nil)
	h.withDocument("field-notes.md", "The aurora appeared over the tundra.", 0.8)
	h.withMemory(h.user, memories.TypePreference, "The user prefers studying in the morning.", 0.75)
	h.notes.byUser[h.user] = []notes.Note{{ID: uuid.New(), Title: "Antenna", Content: "guy-line slack"}}

	turn, _, err := h.send(t, "what should I do today?")
	if err != nil {
		t.Fatal(err)
	}
	got := turn.Assistant.Sources
	if len(got) != 3 {
		t.Fatalf("retrieved %d sources, want document + memory + note: %+v", len(got), got)
	}
	want := []string{SourceDocument, SourceMemory, SourceNote}
	for i, kind := range want {
		if got[i].Type != kind {
			t.Fatalf("source %d is %q, want %q", i, got[i].Type, kind)
		}
		if got[i].Label != label(i) {
			t.Fatalf("source %d is labelled %q, want %q", i, got[i].Label, label(i))
		}
	}
}

// A memory store that errors fails the turn, exactly as a failing document
// search does: answering "I don't know anything about you" because a query
// errored is a false statement about the user's own data.
func TestAFailingMemorySearchFailsTheTurn(t *testing.T) {
	h := newMemoryHarness(t, nil)
	h.mem.err = errors.New("connection reset")

	_, _, err := h.send(t, "what do you remember about me?")
	if err == nil {
		t.Fatal("a failed memory search produced an answer")
	}
	if len(h.store.stored(h.conv.ID)) != 0 {
		t.Fatal("a turn was persisted after retrieval failed")
	}
}

// With no memory service wired the orchestrator is Phase 4 exactly: it
// retrieves documents and items, and remembers nothing.
func TestTheAssistantWorksWithNoMemorySystem(t *testing.T) {
	h := newHarness(t, nil)
	h.withDocument("field-notes.md", "The aurora appeared over the tundra.", 0.8)

	turn, _, err := h.send(t, "what happened over the tundra?")
	if err != nil {
		t.Fatalf("SendMessage without a memory service: %v", err)
	}
	if len(turn.Assistant.Sources) != 1 || turn.Assistant.Sources[0].Type != SourceDocument {
		t.Fatalf("sources = %+v, want the document only", turn.Assistant.Sources)
	}
	if turn.Remembered != nil {
		t.Fatalf("remembered %+v with no extractor wired", turn.Remembered)
	}
}

// --- extraction -------------------------------------------------------------

// The exchange handed to the extractor is the one that was persisted: the
// validated question and the assembled answer, not the raw request body and
// not a partial stream.
func TestTheFinishedTurnIsHandedToTheExtractor(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Reply: "Mornings work best for revision."})
	h.extractor.stored = []memories.Memory{{
		ID: uuid.New(), Type: memories.TypePreference,
		Content: "The user prefers studying in the morning.",
	}}

	turn, _, err := h.send(t, "  when should I schedule my revision?  ")
	if err != nil {
		t.Fatal(err)
	}

	calls := h.extractor.seen()
	if len(calls) != 1 {
		t.Fatalf("the extractor was called %d times, want once per turn", len(calls))
	}
	got := calls[0]
	switch {
	case got.userID != h.user:
		t.Fatalf("extraction ran for %v, want the caller %v", got.userID, h.user)
	case got.convID != h.conv.ID:
		t.Fatalf("extraction recorded conversation %v, want %v", got.convID, h.conv.ID)
	case got.question != turn.User.Content:
		t.Fatalf("extraction saw question %q, want the persisted %q", got.question, turn.User.Content)
	case got.answer != turn.Assistant.Content:
		t.Fatalf("extraction saw answer %q, want the persisted %q", got.answer, turn.Assistant.Content)
	}

	// And what was remembered comes back on the turn, so a client can say so.
	if len(turn.Remembered) != 1 || turn.Remembered[0].Content != "The user prefers studying in the morning." {
		t.Fatalf("Remembered = %+v", turn.Remembered)
	}
}

// The rule the brief is most explicit about: a failure to remember must not
// turn a good answer into an error. The answer is already generated, already
// stored, and already on the user's screen.
func TestExtractionFailureDoesNotFailTheTurn(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Reply: "Mornings work best."})
	h.extractor.err = errors.New("the extraction model timed out")

	turn, sink, err := h.send(t, "when should I schedule my revision?")
	if err != nil {
		t.Fatalf("a failed extraction failed the turn: %v", err)
	}
	if turn.Assistant.Content != "Mornings work best." {
		t.Fatalf("answer = %q", turn.Assistant.Content)
	}
	if sink.Text.String() != turn.Assistant.Content {
		t.Fatalf("streamed %q but stored %q", sink.Text.String(), turn.Assistant.Content)
	}
	if len(turn.Remembered) != 0 {
		t.Fatalf("Remembered = %+v after a failed extraction", turn.Remembered)
	}
	// The turn is on the record either way.
	if len(h.store.stored(h.conv.ID)) != 2 {
		t.Fatal("the turn was not persisted")
	}
}

// Nothing is extracted from a turn that never happened: a refused model call
// persists no messages, so there is no exchange to learn from.
func TestNothingIsExtractedFromAFailedTurn(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Err: ai.ErrUnavailable})

	if _, _, err := h.send(t, "when should I schedule my revision?"); err == nil {
		t.Fatal("a refused model call produced an answer")
	}
	if calls := h.extractor.seen(); len(calls) != 0 {
		t.Fatalf("the extractor ran on a turn that was never persisted: %+v", calls)
	}
}

// A store failure after generation is the same case: the exchange is not on
// the record, so it is not learned from either.
func TestNothingIsExtractedWhenTheTurnCannotBePersisted(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Reply: "Mornings work best."})
	h.store.appendErr = errors.New("connection reset")

	if _, _, err := h.send(t, "when should I schedule my revision?"); err == nil {
		t.Fatal("a failed write produced a turn")
	}
	if calls := h.extractor.seen(); len(calls) != 0 {
		t.Fatalf("the extractor ran on an unpersisted turn: %+v", calls)
	}
}

// Retrieval without extraction is a supported configuration: the assistant uses
// what it already knows and learns nothing new.
func TestRetrievalRunsWithExtractionSwitchedOff(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Reply: "Mornings [S1]."})
	h.svc.extractor = nil
	h.withMemory(h.user, memories.TypePreference, "The user prefers studying in the morning.", 0.8)

	turn, _, err := h.send(t, "when should I schedule my revision?")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Assistant.Sources) != 1 || turn.Assistant.Sources[0].Type != SourceMemory {
		t.Fatalf("sources = %+v, want the memory", turn.Assistant.Sources)
	}
	if len(turn.Remembered) != 0 {
		t.Fatalf("Remembered = %+v with extraction off", turn.Remembered)
	}
}

// --- the prompt -------------------------------------------------------------

// A memory is a claim the assistant recorded, not something the user just
// said, and it can be stale. The system prompt has to say so, or a corrected
// fact loses to the one on file.
func TestTheSystemPromptSaysAMemoryMayBeOutOfDate(t *testing.T) {
	h := newMemoryHarness(t, nil)
	h.withMemory(h.user, memories.TypeSemantic, "The user knows Go.", 0.8)

	if _, _, err := h.send(t, "what languages do I know?"); err != nil {
		t.Fatal(err)
	}
	system := h.provider.LastPrompt()[0].Content
	for _, want := range []string{`"memory"`, "earlier conversation", "out of date"} {
		if !strings.Contains(system, want) {
			t.Fatalf("the system prompt does not mention %q:\n%s", want, system)
		}
	}
}

// The empty-context sentence names memories too, so "nothing was retrieved"
// covers everything retrieval looked at rather than only the Phase 4 sources.
func TestTheEmptyContextBlockNamesMemories(t *testing.T) {
	block := contextBlock(nil)
	if !strings.Contains(block, "memories") {
		t.Fatalf("the empty context block does not mention memories: %q", block)
	}
}

// The extractor is given the turn's own context, so a client that hangs up
// abandons the extraction as well. The exchange is already saved.
func TestExtractionRunsOnTheTurnsContext(t *testing.T) {
	h := newMemoryHarness(t, &ai.Mock{Reply: "Mornings work best."})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := h.svc.SendMessage(ctx, h.user, h.conv.ID, "when should I schedule my revision?", nil); err != nil {
		t.Fatal(err)
	}
	calls := h.extractor.seen()
	if len(calls) != 1 || !calls[0].ctxWasSet {
		t.Fatalf("extraction was called with no context: %+v", calls)
	}
}
