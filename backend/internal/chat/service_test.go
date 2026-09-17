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
	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// harness is one orchestrator with every dependency faked, including the
// model. Nothing here needs Postgres or Ollama.
type harness struct {
	svc      *Service
	store    *fakeStore
	docs     *fakeDocs
	tasks    *fakeTasks
	goals    *fakeGoals
	notes    *fakeNotes
	calendar *fakeEvents
	provider *ai.Mock
	user     uuid.UUID
	conv     Conversation
}

func newHarness(t *testing.T, provider *ai.Mock) *harness {
	t.Helper()
	if provider == nil {
		provider = &ai.Mock{}
	}
	h := &harness{
		store:    newFakeStore(),
		docs:     &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		tasks:    &fakeTasks{byUser: map[uuid.UUID][]tasks.Task{}},
		goals:    &fakeGoals{byUser: map[uuid.UUID][]goals.Goal{}},
		notes:    &fakeNotes{byUser: map[uuid.UUID][]notes.Note{}},
		calendar: &fakeEvents{byUser: map[uuid.UUID][]calendar.Event{}},
		provider: provider,
		user:     uuid.New(),
	}
	h.svc = NewService(Deps{
		Store: h.store, Provider: provider,
		Documents: h.docs, Tasks: h.tasks, Goals: h.goals, Notes: h.notes, Calendar: h.calendar,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// A fixed clock, so the date in the system prompt is assertable.
	h.svc.now = func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }
	h.conv = h.store.seed(h.user)
	return h
}

func (h *harness) withDocument(filename, content string, similarity float64) documents.SearchResult {
	r := documents.SearchResult{
		ChunkID: uuid.New(), DocumentID: uuid.New(), Filename: filename,
		ChunkIndex: 0, Content: content, Similarity: similarity,
	}
	h.docs.byUser[h.user] = append(h.docs.byUser[h.user], r)
	return r
}

func (h *harness) send(t *testing.T, text string) (Turn, *CollectSink, error) {
	t.Helper()
	sink := &CollectSink{}
	turn, err := h.svc.SendMessage(context.Background(), h.user, h.conv.ID, text, sink)
	return turn, sink, err
}

// --- grounding --------------------------------------------------------------

// The property the phase exists for: a question whose answer is in a document
// gets that document's text in the prompt, the answer's citation is recorded,
// and the recorded source points back at the real document.
func TestAnswerIsGroundedInARetrievedDocument(t *testing.T) {
	h := newHarness(t, &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.Contains(ai.PromptText(msgs), "aurora borealis") {
			return "Your field notes record it appearing over the tundra after midnight [S1]."
		}
		return "I could not find anything about that in your data."
	}})
	doc := h.withDocument("field-notes.md",
		"The aurora borealis appeared over the tundra shortly after midnight.", 0.78)

	turn, sink, err := h.send(t, "what happened over the tundra?")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	// The chunk reached the model with its label -- and without its score,
	// which the model would otherwise repeat to the user. The score is on the
	// source, checked below.
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{"aurora borealis", "[S1]", "field-notes.md"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "similarity 0.78") {
		t.Fatalf("prompt leaks the retrieval score:\n%s", prompt)
	}

	// The sources were announced before any token, and recorded on the message.
	if len(sink.Retrieved) != 1 {
		t.Fatalf("sink saw %d sources, want 1", len(sink.Retrieved))
	}
	if len(turn.Assistant.Sources) != 1 {
		t.Fatalf("recorded %d sources, want 1", len(turn.Assistant.Sources))
	}
	got := turn.Assistant.Sources[0]
	switch {
	case got.Type != SourceDocument:
		t.Fatalf("type = %q, want document", got.Type)
	case got.ID != doc.DocumentID:
		t.Fatalf("id = %v, want the document it came from (%v)", got.ID, doc.DocumentID)
	case got.Label != "S1":
		t.Fatalf("label = %q, want S1", got.Label)
	case got.Title != "field-notes.md":
		t.Fatalf("title = %q, want the filename", got.Title)
	case !got.Cited:
		t.Fatal("the answer cited [S1] but the source is recorded as uncited")
	case got.Similarity == nil || *got.Similarity != 0.78:
		t.Fatalf("similarity = %v, want the retrieval score", got.Similarity)
	}
	if sink.Text.String() != turn.Assistant.Content {
		t.Fatalf("streamed %q but stored %q", sink.Text.String(), turn.Assistant.Content)
	}
}

// The other half of the grounding rule: nothing retrieved must produce no
// citation, and the model must be told so explicitly rather than left to
// guess from an absent section.
func TestNoContextIsStatedNotInvented(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "I could not find anything about that in your documents or tasks."})

	turn, sink, err := h.send(t, "what is my bank balance?")
	if err != nil {
		t.Fatal(err)
	}

	prompt := ai.PromptText(h.provider.LastPrompt())
	if !strings.Contains(prompt, "Nothing relevant was found") {
		t.Fatalf("prompt does not state that retrieval found nothing:\n%s", prompt)
	}
	if len(sink.Retrieved) != 0 {
		t.Fatalf("sink saw %d sources, want none", len(sink.Retrieved))
	}
	// Stored as SQL NULL rather than an empty array: "grounded in nothing" is
	// a fact worth keeping distinct from "never grounded".
	if turn.Assistant.Sources != nil {
		t.Fatalf("sources = %v, want nil", turn.Assistant.Sources)
	}
}

// Retrieval hands the model five things; an answer typically uses one.
// Recording all five as used would make the citation trail worthless.
func TestUnusedSourcesAreRecordedAsUncited(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "According to [S2], yes."})
	h.withDocument("a.md", "first", 0.7)
	h.withDocument("b.md", "second", 0.6)
	h.withDocument("c.md", "third", 0.55)

	turn, _, err := h.send(t, "which one?")
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Assistant.Sources) != 3 {
		t.Fatalf("recorded %d sources, want all 3 retrieved", len(turn.Assistant.Sources))
	}
	for i, s := range turn.Assistant.Sources {
		want := s.Label == "S2"
		if s.Cited != want {
			t.Fatalf("source %d (%s) cited = %v, want %v", i, s.Label, s.Cited, want)
		}
	}
}

// The rules the master spec requires have to actually be in the prompt.
func TestSystemPromptCarriesTheGroundingAndActionRules(t *testing.T) {
	h := newHarness(t, nil)
	if _, _, err := h.send(t, "hello"); err != nil {
		t.Fatal(err)
	}

	prompt := h.provider.LastPrompt()
	if prompt[0].Role != ai.RoleSystem {
		t.Fatalf("first message role = %q, want system", prompt[0].Role)
	}
	rules := prompt[0].Content
	for _, want := range []string{
		"Cite only labels that appear in the context",
		"Never say that something is in the user's documents",
		"say so plainly",
		"cannot create, update or delete",
		"9 September 2026", // the current date, for "what is due next"
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("the system prompt is missing %q:\n%s", want, rules)
		}
	}
}

// An assistant with no action engine must not be able to reach a writer, and
// the closest thing to a test for that is structural: the orchestrator is
// given readers only. This pins the read-only surface so adding a writer to
// Deps is a deliberate act rather than an accident.
func TestOrchestratorIsGivenReadersOnly(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "Taking actions is not supported yet."})
	if _, _, err := h.send(t, "create a task called ship phase 5"); err != nil {
		t.Fatal(err)
	}
	// The task lister was asked to list, and nothing else exists to call.
	for _, f := range h.tasks.filters {
		if f.Limit <= 0 {
			t.Fatalf("task retrieval used %+v, want a bounded list", f)
		}
	}
	if n := len(h.store.stored(h.conv.ID)); n != 2 {
		t.Fatalf("stored %d messages, want exactly the question and the answer", n)
	}
}

// --- isolation --------------------------------------------------------------

// Every retrieval and every write must be made in the caller's name. This is
// the unit-level half of the cross-user property; the SQL half is in
// internal/api/isolation_test.go.
func TestEveryDependencyIsCalledWithTheCallersID(t *testing.T) {
	h := newHarness(t, nil)
	h.withDocument("mine.md", "text", 0.7)
	if _, _, err := h.send(t, "anything?"); err != nil {
		t.Fatal(err)
	}

	groups := map[string][]uuid.UUID{
		"documents":     h.docs.callers,
		"tasks":         h.tasks.callers,
		"goals":         h.goals.callers,
		"notes":         h.notes.callers,
		"conversations": h.store.callersSeen(),
	}
	for name, callers := range groups {
		if len(callers) == 0 {
			t.Fatalf("%s was never consulted", name)
		}
		for _, got := range callers {
			if got != h.user {
				t.Fatalf("%s was called with %v, want the authenticated user %v", name, got, h.user)
			}
		}
	}
}

func TestAnotherUsersConversationIsNotFound(t *testing.T) {
	h := newHarness(t, nil)
	stranger := uuid.New()

	_, err := h.svc.SendMessage(context.Background(), stranger, h.conv.ID, "hello", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(h.provider.Calls()) != 0 {
		t.Fatal("a foreign conversation reached the model")
	}
	if n := len(h.store.stored(h.conv.ID)); n != 0 {
		t.Fatalf("%d messages were written to another user's conversation", n)
	}
}

// --- failure modes ----------------------------------------------------------

// A turn that did not finish is not stored at all. The alternative -- keeping
// the question -- leaves a dangling user message the next turn replays as
// history and a client cannot distinguish from an unanswered question.
func TestAFailedTurnPersistsNothing(t *testing.T) {
	outage := errors.New("connection refused")
	for _, tc := range []struct {
		name     string
		provider *ai.Mock
		docsErr  error
	}{
		{"model refuses the call", &ai.Mock{Err: outage}, nil},
		{"generation is cut off", &ai.Mock{Reply: "half an answer here", StreamErr: outage}, nil},
		{"model returns nothing", &ai.Mock{ReplyFunc: func([]ai.Message) string { return "  " }}, nil},
		{"retrieval fails", &ai.Mock{}, documents.ErrEmbedding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.provider)
			h.docs.err = tc.docsErr

			if _, _, err := h.send(t, "a question"); err == nil {
				t.Fatal("want an error")
			}
			if n := len(h.store.stored(h.conv.ID)); n != 0 {
				t.Fatalf("stored %d messages after a failed turn, want 0", n)
			}
		})
	}
}

// An embedding outage during retrieval is an outage, not a 500 -- and it must
// not be answered as "I found nothing in your documents", which would be a
// false statement about the user's data.
func TestRetrievalOutageIsReportedAsUnavailable(t *testing.T) {
	h := newHarness(t, nil)
	h.docs.err = documents.ErrEmbedding

	_, _, err := h.send(t, "what did I write about the tundra?")
	if !Unavailable(err) {
		t.Fatalf("err = %v, want an unavailable error", err)
	}
	if len(h.provider.Calls()) != 0 {
		t.Fatal("the model was asked to answer from context that could not be retrieved")
	}
}

func TestModelOutageIsReportedAsUnavailable(t *testing.T) {
	h := newHarness(t, &ai.Mock{Err: ai.ErrUnavailable})
	if _, _, err := h.send(t, "hello"); !Unavailable(err) {
		t.Fatalf("err = %v, want an unavailable error", err)
	}
}

// A client that hangs up mid-answer abandons the turn rather than having it
// finish and be stored behind their back.
func TestASinkFailureAbandonsTheTurn(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "one two three four"})
	gone := errors.New("client disconnected")

	_, err := h.svc.SendMessage(context.Background(), h.user, h.conv.ID, "hi", &failingSink{after: 1, err: gone})
	if !errors.Is(err, gone) {
		t.Fatalf("err = %v, want the sink's error", err)
	}
	if n := len(h.store.stored(h.conv.ID)); n != 0 {
		t.Fatalf("stored %d messages for an abandoned turn, want 0", n)
	}
}

type failingSink struct {
	after int
	err   error
	n     int
}

func (s *failingSink) Sources([]Source) error     { return nil }
func (s *failingSink) Actions([]TurnAction) error { return nil }
func (s *failingSink) Token(string) error {
	s.n++
	if s.n > s.after {
		return s.err
	}
	return nil
}

func TestValidationRejectsAnEmptyOrHugeMessage(t *testing.T) {
	h := newHarness(t, nil)
	for _, tc := range []struct{ name, content string }{
		{"empty", ""},
		{"whitespace", "   \n\t "},
		{"too long", strings.Repeat("a", MaxMessageLen+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.send(t, tc.content)
			var verrs validate.Errors
			if !errors.As(err, &verrs) || verrs[0].Field != "content" {
				t.Fatalf("err = %v, want a validation error on content", err)
			}
			if len(h.provider.Calls()) != 0 {
				t.Fatal("an invalid message reached the model")
			}
		})
	}
}

// --- history and titles -----------------------------------------------------

func TestHistoryIsReplayedAsTurnsAndBounded(t *testing.T) {
	h := newHarness(t, nil)
	// More history than the window, so the oldest must fall out of it.
	for i := 0; i < MaxHistoryMessages; i++ {
		if _, _, err := h.send(t, "question "+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}

	prompt := h.provider.LastPrompt()
	replayed := 0
	for _, m := range prompt {
		if m.Role == ai.RoleUser || m.Role == ai.RoleAssistant {
			replayed++
		}
	}
	// The window, plus the new question that is not part of it.
	if replayed != MaxHistoryMessages+1 {
		t.Fatalf("replayed %d turns, want %d", replayed, MaxHistoryMessages+1)
	}
	if strings.Contains(ai.PromptText(prompt), "question a") {
		t.Fatal("the oldest message survived a window that should have dropped it")
	}
	// The context block sits immediately before the new question, not at the
	// top: it was retrieved for this question.
	last := prompt[len(prompt)-1]
	if last.Role != ai.RoleUser {
		t.Fatalf("last prompt message is %q, want the user's question", last.Role)
	}
	if before := prompt[len(prompt)-2]; before.Role != ai.RoleSystem || !strings.HasPrefix(before.Content, "CONTEXT") {
		t.Fatalf("the message before the question is %q/%q, want the context block", before.Role, before.Content)
	}
}

func TestFirstMessageNamesTheConversation(t *testing.T) {
	h := newHarness(t, nil)
	if _, _, err := h.send(t, "  What did I write about\nthe aurora?  "); err != nil {
		t.Fatal(err)
	}
	conv, err := h.svc.Get(context.Background(), h.user, h.conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Title != "What did I write about" {
		t.Fatalf("title = %q, want the first line of the first message", conv.Title)
	}

	// A second message leaves the name alone.
	if _, _, err := h.send(t, "and the tundra?"); err != nil {
		t.Fatal(err)
	}
	conv, _ = h.svc.Get(context.Background(), h.user, h.conv.ID)
	if conv.Title != "What did I write about" {
		t.Fatalf("title = %q, want it unchanged by the second message", conv.Title)
	}
}

func TestAnExplicitTitleSurvivesTheFirstMessage(t *testing.T) {
	h := newHarness(t, nil)
	conv, err := h.svc.Create(context.Background(), h.user, "Thesis planning")
	if err != nil {
		t.Fatal(err)
	}
	h.conv = conv
	if _, _, err := h.send(t, "where do I start?"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.Get(context.Background(), h.user, conv.ID)
	if got.Title != "Thesis planning" {
		t.Fatalf("title = %q, want the one the user chose", got.Title)
	}
}

// --- the tasks/goals/notes heuristic ----------------------------------------

// Documents are matched semantically; Phase 2 items are not, because they have
// no embeddings yet. What they get instead is a stated heuristic, and this
// pins it so a later change to it is deliberate.
func TestPhase2RetrievalUsesTheStatedHeuristic(t *testing.T) {
	h := newHarness(t, nil)
	if _, _, err := h.send(t, "what should I do next?"); err != nil {
		t.Fatal(err)
	}

	if len(h.tasks.filters) != 2 {
		t.Fatalf("made %d task queries, want 2 (in progress, then due soonest)", len(h.tasks.filters))
	}
	if got := h.tasks.filters[0]; got.Status != "in_progress" || got.Sort != "-updated_at" {
		t.Fatalf("first task query = %+v, want in_progress by recency", got)
	}
	if got := h.tasks.filters[1]; got.Status != "pending" || got.Sort != "deadline" {
		t.Fatalf("second task query = %+v, want pending by deadline", got)
	}
	if got := h.goals.filters[0]; got.Status != "active" || got.Sort != "deadline" {
		t.Fatalf("goal query = %+v, want active goals by deadline", got)
	}
	if got := h.notes.filters[0]; got.Sort != "-updated_at" {
		t.Fatalf("note query = %+v, want recently updated notes", got)
	}
	// And the retrieval floor is applied to the document search, not left at
	// zero, which is what stops an unrelated question citing a document.
	if got := h.docs.queries[0]; got.MinSimilarity != DefaultMinSimilarity || got.Limit != MaxDocumentChunks {
		t.Fatalf("document query = %+v, want the floor and the top-k applied", got)
	}
}

func TestRetrievedItemsCarryTheFieldsAnAnswerNeeds(t *testing.T) {
	h := newHarness(t, nil)
	due := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	desc := "Draft the retrieval section."
	h.tasks.byUser[h.user] = []tasks.Task{{
		ID: uuid.New(), Title: "Ship phase 4", Priority: "high", Status: "pending",
		Deadline: &due, Description: &desc, Tags: []string{"api"},
	}}
	h.goals.byUser[h.user] = []goals.Goal{{
		ID: uuid.New(), Title: "Finish the degree", Type: "education", Status: "active",
		Milestones: []goals.Milestone{{Completed: true}, {}},
	}}
	h.notes.byUser[h.user] = []notes.Note{{
		ID: uuid.New(), Title: "Reading list", Content: "Attention is all you need.", Tags: []string{"ml"},
	}}

	turn, _, err := h.send(t, "what is next?")
	if err != nil {
		t.Fatal(err)
	}

	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		"Ship phase 4", "priority high", "due 2026-09-20", "Draft the retrieval section.",
		"Finish the degree", "milestones 1 of 2 complete",
		"Reading list", "Attention is all you need.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	// Labels are dense and ordered, so a citation can name one.
	for i, s := range turn.Assistant.Sources {
		if s.Label != label(i) {
			t.Fatalf("source %d is labelled %q, want %q", i, s.Label, label(i))
		}
	}
	if len(turn.Assistant.Sources) != 3 {
		t.Fatalf("recorded %d sources, want one per retrieved item", len(turn.Assistant.Sources))
	}
}

// --- conversation CRUD ------------------------------------------------------

func TestConversationLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	conv, err := h.svc.Create(context.Background(), h.user, "")
	if err != nil {
		t.Fatal(err)
	}
	if conv.Title != DefaultTitle {
		t.Fatalf("title = %q, want the default", conv.Title)
	}

	list, err := h.svc.List(context.Background(), h.user, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 { // the seeded one and this one
		t.Fatalf("listed %d conversations, want 2", len(list))
	}

	if err := h.svc.Delete(context.Background(), h.user, conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Get(context.Background(), h.user, conv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound after delete", err)
	}
	if err := h.svc.Delete(context.Background(), h.user, conv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestGetReturnsTheTurnsInOrder(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "An answer."})
	if _, _, err := h.send(t, "A question?"); err != nil {
		t.Fatal(err)
	}
	conv, err := h.svc.Get(context.Background(), h.user, h.conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(conv.Messages))
	}
	if conv.Messages[0].Role != RoleUser || conv.Messages[1].Role != RoleAssistant {
		t.Fatalf("roles = %q, %q -- the answer must not precede the question",
			conv.Messages[0].Role, conv.Messages[1].Role)
	}
	if conv.Messages[0].Sources != nil {
		t.Fatal("a user message must carry no sources")
	}
}
