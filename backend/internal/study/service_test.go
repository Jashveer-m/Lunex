package study

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness is one service over fresh fakes, plus the owner every call is made
// as. Nothing here ever names a second user except to prove it cannot be
// reached.
type harness struct {
	svc     *Service
	store   *fakeStore
	library *fakeLibrary
	graph   *fakeSyncer
	model   *ai.Mock
	owner   uuid.UUID
	docID   uuid.UUID
}

func newHarness(t *testing.T, reply string) *harness {
	t.Helper()
	h := &harness{
		store: newFakeStore(), library: newFakeLibrary(), graph: &fakeSyncer{},
		model: &ai.Mock{Reply: reply}, owner: uuid.New(),
	}
	// One document, in both fakes: the library holds its text, and the store
	// holds the ownership probe the write path runs.
	h.docID = h.store.seedDocument(h.owner, "field-notes.txt")
	h.library.seed(h.owner, h.docID, "field-notes.txt", auroraPassage, generatorPassage)
	h.svc = NewService(Deps{
		Store: h.store, Library: h.library, Provider: h.model,
		Graph: h.graph, Logger: quiet(),
	})
	return h
}

// The reply a well-behaved model gives for the seeded document.
const groundedReply = `[{"front":"How long did the aurora borealis last?","back":"About forty minutes."},` +
	`{"front":"What does the generator need before the next resupply run?","back":"A new fuel filter."}]`

// --- plans ----------------------------------------------------------------------

func TestCreatePlanStoresWhatWasValidated(t *testing.T) {
	h := newHarness(t, groundedReply)

	p, err := h.svc.CreatePlan(context.Background(), h.owner, CreatePlanInput{
		Title: "  Linear algebra finals  ", Description: " Eigenvalues and SVD ",
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case p.Title != "Linear algebra finals":
		t.Fatalf("title = %q, want it trimmed", p.Title)
	case p.Description == nil || *p.Description != "Eigenvalues and SVD":
		t.Fatalf("description = %v, want it trimmed", p.Description)
	case p.Status != StatusActive:
		t.Fatalf("status = %q, want a new plan to be active", p.Status)
	case p.CardCount != 0:
		t.Fatalf("a new plan has %d cards", p.CardCount)
	}
}

func TestAPlanCanBeBuiltFromADocumentTheCallerOwns(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()

	p, err := h.svc.CreatePlan(ctx, h.owner, CreatePlanInput{Title: "Field notes", DocumentID: &h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if p.DocumentID == nil || *p.DocumentID != h.docID {
		t.Fatalf("document_id = %v, want the document", p.DocumentID)
	}
	if p.DocumentName == nil || *p.DocumentName != "field-notes.txt" {
		t.Fatalf("document name = %v, want it joined on read", p.DocumentName)
	}
}

// A document somebody else owns is a 404, not a validation error naming the
// field: the two answers differ, and only the first keeps another user's ids
// unconfirmable.
func TestAPlanCannotBeBuiltFromAForeignDocument(t *testing.T) {
	h := newHarness(t, groundedReply)
	stranger := uuid.New()
	theirs := h.store.seedDocument(stranger, "their-notes.txt")

	_, err := h.svc.CreatePlan(context.Background(), h.owner, CreatePlanInput{
		Title: "Borrowed", DocumentID: &theirs,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if others := h.store.otherUsers(h.owner); len(others) > 0 {
		t.Fatalf("the store was called for %v", others)
	}
}

func TestPlansAreScopedToTheCaller(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	if _, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription()); err != nil {
		t.Fatal(err)
	}

	stranger := uuid.New()
	found, err := h.svc.Plans(ctx, stranger, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("another user sees %d plans", len(found))
	}
	mine, err := h.svc.Plans(ctx, h.owner, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Fatalf("the owner sees %d plans, want 1", len(mine))
	}
}

func TestUpdatePlanAppliesOnlyWhatTheBodyMentions(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	created, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}

	updated, err := h.svc.UpdatePlan(ctx, h.owner, created.ID, UpdatePlanInput{
		Status: optional.Of(StatusCompleted),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusCompleted {
		t.Fatalf("status = %q", updated.Status)
	}
	if updated.Title != created.Title || updated.Description == nil {
		t.Fatalf("an untouched field changed: %+v", updated)
	}

	// And an explicit null clears the nullable column.
	cleared, err := h.svc.UpdatePlan(ctx, h.owner, created.ID, clearedDescription())
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Description != nil {
		t.Fatalf("description = %v, want it cleared", *cleared.Description)
	}
}

func TestAnUnknownStatusIsAFieldError(t *testing.T) {
	h := newHarness(t, groundedReply)
	_, err := h.svc.CreatePlan(context.Background(), h.owner, CreatePlanInput{
		Title: "Revision", Status: "studying",
	})
	var verrs validate.Errors
	if !errors.As(err, &verrs) || verrs[0].Field != "status" {
		t.Fatalf("err = %v, want a field error on status", err)
	}
}

// --- flashcards --------------------------------------------------------------------

func TestFlashcardsAreListedUnderTheirPlan(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddFlashcard(ctx, h.owner, plan.ID, NewCard{
		Front: "What is an eigenvalue?", Back: "A scalar λ with Av = λv.",
	}); err != nil {
		t.Fatal(err)
	}

	cards, err := h.svc.Flashcards(ctx, h.owner, plan.ID, CardFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("the plan has %d cards, want 1", len(cards))
	}
	// A card the user typed claims no source. Borrowing the plan's document
	// would assert a provenance the card does not have, which is the one thing
	// document_id is for.
	if cards[0].DocumentID != nil {
		t.Fatalf("a hand-written card names a source document: %v", cards[0].DocumentID)
	}
	// And the plan now says so without a second query.
	reloaded, err := h.svc.Plan(ctx, h.owner, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CardCount != 1 {
		t.Fatalf("card_count = %d, want 1", reloaded.CardCount)
	}
}

// "No cards" and "no such plan" are different answers, and only the first is
// true of an empty plan somebody owns.
func TestListingAForeignPlansCardsIsNotFound(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.Flashcards(ctx, uuid.New(), plan.ID, CardFilter{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.AddFlashcard(ctx, uuid.New(), plan.ID, NewCard{Front: "q", Back: "a"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adding to a foreign plan: err = %v, want ErrNotFound", err)
	}
}

func TestACardNeedsBothSides(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	for name, card := range map[string]NewCard{
		"no back":  {Front: "What is an eigenvalue?"},
		"no front": {Back: "A scalar."},
		"neither":  {},
		"blank":    {Front: "   ", Back: "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.svc.AddFlashcard(ctx, h.owner, plan.ID, card); err == nil {
				t.Fatal("a half-written card was accepted")
			}
		})
	}
}

func TestDeletingAPlanTakesItsCards(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddFlashcard(ctx, h.owner, plan.ID, NewCard{Front: "q?", Back: "a."}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeletePlan(ctx, h.owner, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Flashcards(ctx, h.owner, plan.ID, CardFilter{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the plan is still there: %v", err)
	}
	if n := len(h.store.cards[h.owner]); n != 0 {
		t.Fatalf("%d cards outlived their plan", n)
	}
}

// --- generation ----------------------------------------------------------------------

// ProposeFlashcards writes nothing. That is the approval story in one test:
// the cards exist as a proposal and the store has not been asked to insert
// anything.
func TestProposingFlashcardsWritesNothing(t *testing.T) {
	h := newHarness(t, groundedReply)

	proposal, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Cards) != 2 {
		t.Fatalf("proposed %d cards, want 2: %+v", len(proposal.Cards), proposal.Cards)
	}
	if len(h.store.batches) != 0 {
		t.Fatalf("a proposal wrote %d batches of cards", len(h.store.batches))
	}
	if proposal.Filename != "field-notes.txt" || proposal.DocumentID != h.docID {
		t.Fatalf("the proposal does not name its source: %+v", proposal)
	}
	// The passages it used are named, so the same check anybody else runs is
	// the check it ran.
	if len(proposal.ChunkIndexes) != 2 {
		t.Fatalf("chunk indexes = %v, want both passages", proposal.ChunkIndexes)
	}
}

// The grounding test the phase exists for: a card whose answer is not in the
// document does not become a card.
func TestAnInventedFlashcardIsDropped(t *testing.T) {
	const reply = `[
	  {"front":"How long did the aurora borealis last?","back":"About forty minutes."},
	  {"front":"What is the capital of Mongolia?","back":"Ulaanbaatar, founded in 1639."},
	  {"front":"What fuel does the generator burn?","back":"Marine diesel, at nine litres an hour."}
	]`
	h := newHarness(t, reply)

	proposal, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Cards) != 1 {
		t.Fatalf("kept %d cards, want only the grounded one: %+v", len(proposal.Cards), proposal.Cards)
	}
	if !strings.Contains(proposal.Cards[0].Back, "forty minutes") {
		t.Fatalf("the kept card is %+v", proposal.Cards[0])
	}
	if proposal.Dropped != 2 {
		t.Fatalf("dropped = %d, want 2 -- and it is reported, not swallowed", proposal.Dropped)
	}
}

// Every card being invented is not a thin proposal, it is no proposal: the
// user must not be shown cards to approve when none of them are about their
// document.
func TestAWhollyInventedBatchIsRefused(t *testing.T) {
	h := newHarness(t, `[{"front":"What is the capital of Mongolia?","back":"Ulaanbaatar."}]`)

	_, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID})
	if !errors.Is(err, ErrGeneration) {
		t.Fatalf("err = %v, want ErrGeneration", err)
	}
}

// The cards are checked against the passages the model was actually shown, and
// those passages are what the prompt carried.
func TestTheModelIsShownTheDocumentsOwnText(t *testing.T) {
	h := newHarness(t, groundedReply)
	if _, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID}); err != nil {
		t.Fatal(err)
	}
	body := lastUserMessage(t, h.model)
	for _, want := range []string{"aurora borealis", "fuel filter", "field-notes.txt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, body)
		}
	}
	// And it is told where the content must come from, in so many words.
	if !strings.Contains(ai.PromptText(h.model.LastPrompt()), "must be stated in the passages") {
		t.Fatal("the prompt does not state the grounding rule")
	}
}

// The prompt's own worked example must share no vocabulary with the document
// being studied. If it did, a model that copied the example's cards verbatim
// would sail through the grounding check, and the check would be measuring
// nothing at all.
func TestTheWorkedExampleIsAboutSomethingElse(t *testing.T) {
	example := SignificantWords(examplePassages + " " + exampleReply)
	for _, w := range SignificantWords(auroraPassage + " " + generatorPassage) {
		for _, e := range example {
			if sameWord(e, w) {
				t.Fatalf("the prompt's example shares %q with the test document, "+
					"so a copied card would pass the grounding check", w)
			}
		}
	}
}

// lastUserMessage is the half of the prompt that carries the document, as
// opposed to the system half that carries the instructions.
func lastUserMessage(t *testing.T, m *ai.Mock) string {
	t.Helper()
	msgs := m.LastPrompt()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleUser {
			return msgs[i].Content
		}
	}
	t.Fatal("the prompt has no user message")
	return ""
}

func TestATopicNarrowsToThePassagesThatMatch(t *testing.T) {
	h := newHarness(t, groundedReply)

	if _, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{
		DocumentID: h.docID, Topic: "generator",
	}); err != nil {
		t.Fatal(err)
	}
	if len(h.library.queries) != 1 {
		t.Fatalf("a topic made %d searches, want 1", len(h.library.queries))
	}
	q := h.library.queries[0]
	if q.Query != "generator" || len(q.DocumentIDs) != 1 || q.DocumentIDs[0] != h.docID {
		t.Fatalf("the search was not restricted to the named document: %+v", q)
	}
	// The document half of the prompt, not the system half -- whose worked
	// example is about something else entirely, on purpose.
	body := lastUserMessage(t, h.model)
	if strings.Contains(body, "aurora borealis") {
		t.Fatalf("a topic-narrowed prompt carries the rest of the document:\n%s", body)
	}
	if !strings.Contains(body, "fuel filter") {
		t.Fatalf("the narrowed prompt lost the passage it matched:\n%s", body)
	}
}

// A topic the document does not cover is refused, not quietly answered with
// the start of the document -- which would make cards about something the user
// did not ask for and give them no way to see it had happened.
func TestATopicThatMatchesNothingIsRefused(t *testing.T) {
	h := newHarness(t, groundedReply)

	_, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{
		DocumentID: h.docID, Topic: "photosynthesis",
	})
	if !errors.Is(err, ErrNoPassages) {
		t.Fatalf("err = %v, want ErrNoPassages", err)
	}
	if len(h.model.Calls()) != 0 {
		t.Fatal("the model was called for a topic the document does not cover")
	}
}

func TestGeneratingFromAForeignDocumentIsNotFound(t *testing.T) {
	h := newHarness(t, groundedReply)
	stranger := uuid.New()
	theirID := uuid.New()
	h.library.seed(stranger, theirID, "their-notes.txt", auroraPassage)

	_, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: theirID})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(h.model.Calls()) != 0 {
		t.Fatal("the model was called for somebody else's document")
	}
}

func TestADocumentWithNoIndexedTextIsRefusedBeforeTheModel(t *testing.T) {
	h := newHarness(t, groundedReply)
	for name, set := range map[string]func(){
		"still processing": func() { h.library.setStatus(h.owner, h.docID, documents.StatusProcessing, 0) },
		"failed":           func() { h.library.setStatus(h.owner, h.docID, documents.StatusFailed, 0) },
		"no chunks":        func() { h.library.setStatus(h.owner, h.docID, documents.StatusReady, 0) },
	} {
		t.Run(name, func(t *testing.T) {
			set()
			_, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID})
			if !errors.Is(err, ErrNotStudyable) {
				t.Fatalf("err = %v, want ErrNotStudyable", err)
			}
		})
	}
	if len(h.model.Calls()) != 0 {
		t.Fatal("the model was called for a document with no text")
	}
}

func TestTheCountIsClampedRatherThanRefused(t *testing.T) {
	h := newHarness(t, groundedReply)

	if _, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{
		DocumentID: h.docID, Count: 500,
	}); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.model.LastPrompt())
	if !strings.Contains(prompt, "Write up to 20 flashcards") {
		t.Fatalf("the count was not clamped to %d:\n%s", MaxCardsPerBatch, prompt)
	}
}

// Creating the cards is a separate call, and it is the only one that writes.
// What it takes is the cards -- not a document and a count -- so there is no
// way for an approval to regenerate.
func TestCreateFlashcardsWritesExactlyWhatItIsGiven(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()
	plan, err := h.svc.CreatePlan(ctx, h.owner, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := h.svc.ProposeFlashcards(ctx, h.owner, GenerateInput{
		DocumentID: h.docID, StudyPlanID: &plan.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	in := make([]CreateCardInput, 0, len(proposal.Cards))
	for _, c := range proposal.Cards {
		docID := proposal.DocumentID
		in = append(in, CreateCardInput{
			StudyPlanID: &plan.ID, DocumentID: &docID, Front: c.Front, Back: c.Back,
		})
	}
	created, err := h.svc.CreateFlashcards(ctx, h.owner, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != len(proposal.Cards) {
		t.Fatalf("created %d cards from a proposal of %d", len(created), len(proposal.Cards))
	}
	for i, c := range created {
		if c.Front != proposal.Cards[i].Front || c.Back != proposal.Cards[i].Back {
			t.Fatalf("card %d was stored as %+v, want %+v", i, c, proposal.Cards[i])
		}
		if c.DocumentID == nil || *c.DocumentID != h.docID {
			t.Fatalf("card %d does not record where it came from: %+v", i, c)
		}
	}
	// One model call, made when the proposal was prepared, and none since.
	if n := len(h.model.Calls()); n != 1 {
		t.Fatalf("the model was called %d times, want 1 -- approving must not regenerate", n)
	}
}

func TestCreateFlashcardsRefusesAForeignPlan(t *testing.T) {
	h := newHarness(t, groundedReply)
	stranger := uuid.New()
	their, err := h.svc.CreatePlan(context.Background(), stranger, planWithDescription())
	if err != nil {
		t.Fatal(err)
	}

	_, err = h.svc.CreateFlashcards(context.Background(), h.owner, []CreateCardInput{{
		StudyPlanID: &their.ID, Front: "q?", Back: "a.",
	}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestABatchIsBounded(t *testing.T) {
	h := newHarness(t, groundedReply)
	in := make([]CreateCardInput, MaxCardsPerBatch+1)
	for i := range in {
		in[i] = CreateCardInput{Front: "q?", Back: "a."}
	}
	if _, err := h.svc.CreateFlashcards(context.Background(), h.owner, in); err == nil {
		t.Fatalf("a batch of %d was accepted", len(in))
	}
	if _, err := h.svc.CreateFlashcards(context.Background(), h.owner, nil); err == nil {
		t.Fatal("an empty batch was accepted")
	}
}

// --- resolving a document ------------------------------------------------------------

func TestResolveDocumentMatchesIdExactNameThenSubstring(t *testing.T) {
	h := newHarness(t, groundedReply)
	ctx := context.Background()

	for name, ref := range map[string]string{
		"by id":        h.docID.String(),
		"by name":      "field-notes.txt",
		"by case":      "FIELD-NOTES.TXT",
		"by substring": "field",
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := h.svc.ResolveDocument(ctx, h.owner, ref)
			if err != nil {
				t.Fatal(err)
			}
			if doc.ID != h.docID {
				t.Fatalf("resolved to %v", doc.ID)
			}
		})
	}
	if _, err := h.svc.ResolveDocument(ctx, h.owner, "lecture-3.pdf"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// Another user's document is the same "no such document".
	stranger := uuid.New()
	h.library.seed(stranger, uuid.New(), "their-notes.txt", auroraPassage)
	if _, err := h.svc.ResolveDocument(ctx, h.owner, "their-notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want another user's document to be unresolvable", err)
	}
}

// Guessing which file to build a deck from is exactly the decision the user
// should be making: the cards from the wrong lecture will look perfectly well
// made.
func TestAnAmbiguousDocumentReferenceNamesTheCandidates(t *testing.T) {
	h := newHarness(t, groundedReply)
	h.library.seed(h.owner, uuid.New(), "lecture-3-notes.txt", auroraPassage)

	_, err := h.svc.ResolveDocument(context.Background(), h.owner, "notes")
	var ambiguous *AmbiguousDocumentError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want an ambiguity", err)
	}
	if len(ambiguous.Matches) != 2 {
		t.Fatalf("matches = %v, want both files named", ambiguous.Matches)
	}
}

// --- wiring ----------------------------------------------------------------------------

func TestWithoutAModelGenerationFailsAndTheRestWorks(t *testing.T) {
	store := newFakeStore()
	svc := NewService(Deps{Store: store, Logger: quiet()})
	ctx, owner := context.Background(), uuid.New()

	if _, err := svc.CreatePlan(ctx, owner, planWithDescription()); err != nil {
		t.Fatalf("a service with no model could not create a plan: %v", err)
	}
	if _, err := svc.ProposeFlashcards(ctx, owner, GenerateInput{DocumentID: uuid.New()}); !errors.Is(err, ErrGeneration) {
		t.Fatalf("err = %v, want ErrGeneration", err)
	}
}

func TestAModelOutageIsClassifiedAsAGenerationFailure(t *testing.T) {
	h := newHarness(t, groundedReply)
	h.model.Err = ai.ErrUnavailable

	_, err := h.svc.ProposeFlashcards(context.Background(), h.owner, GenerateInput{DocumentID: h.docID})
	if !errors.Is(err, ErrGeneration) || !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("err = %v, want both ErrGeneration and the outage", err)
	}
}
