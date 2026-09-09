package memories

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// harness is one memory service with every dependency faked, including the
// model. Nothing here needs Postgres or Ollama.
type harness struct {
	svc      *Service
	store    *fakeStore
	embedder *fakeEmbedder
	provider *ai.Mock
	user     uuid.UUID
	conv     uuid.UUID
}

func newHarness(t *testing.T, provider *ai.Mock) *harness {
	t.Helper()
	if provider == nil {
		provider = &ai.Mock{}
	}
	h := &harness{
		store:    newFakeStore(),
		embedder: &fakeEmbedder{},
		provider: provider,
		user:     uuid.New(),
		conv:     uuid.New(),
	}
	h.svc = NewService(Deps{
		Store: h.store, Provider: provider, Embedder: h.embedder,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// jsonProvider is a MockProvider that answers extraction calls with the facts
// it is given, as the JSON the prompt asks for.
func jsonProvider(reply string) *ai.Mock {
	return &ai.Mock{Reply: reply}
}

// A substantial exchange, used wherever the test is about something other than
// the length threshold.
const (
	realQuestion = "I am halfway through the operating systems course and I keep putting off the scheduler assignment."
	realAnswer   = "You have the scheduler assignment listed as in progress with a deadline on Friday, and nothing else is due before it."
)

// --- extraction quality -----------------------------------------------------

// The pipeline over a few example conversations: what the model proposes, what
// survives the confidence floor and the cap, and what is finally written.
//
// The model here is deterministic, so what this measures is the pipeline's own
// judgement -- the threshold, the filtering, the scoring, the provenance --
// rather than whether llama3.2 understands a sentence. That question is asked
// against a real model in scripts/e2e.sh, which is where it can be answered
// honestly.
func TestExtractionQualityOverExampleConversations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		user      string
		assistant string
		reply     string
		want      []Candidate
		wantCalls int
	}{
		{
			name: "a stated preference is remembered",
			user: "I prefer studying in the morning before class, so please keep my revision blocks early in the day.",
			assistant: "Understood. Your calendar is clear before 09:00 on weekdays, so early revision blocks fit " +
				"every day this week.",
			reply: `[{"type":"preference","content":"The user prefers studying in the morning before class.","importance":0.8,"confidence":0.9}]`,
			want: []Candidate{{
				Type: TypePreference, Content: "The user prefers studying in the morning before class.",
				Importance: 0.8, Confidence: 0.9,
			}},
			wantCalls: 1,
		},
		{
			name:      "a standing fact and something that happened, from one exchange",
			user:      "I finished the OS assignment last night. I wrote it in Go, which I have used for about three years.",
			assistant: "Nice work. That clears the only thing that was due this week.",
			reply: `[
				{"type":"episodic","content":"The user finished the operating systems assignment on 9 September 2026.","importance":0.6,"confidence":0.95},
				{"type":"semantic","content":"The user has used Go for about three years.","importance":0.8,"confidence":0.9}
			]`,
			want: []Candidate{
				{Type: TypeEpisodic, Content: "The user finished the operating systems assignment on 9 September 2026.", Importance: 0.6, Confidence: 0.95},
				{Type: TypeSemantic, Content: "The user has used Go for about three years.", Importance: 0.8, Confidence: 0.9},
			},
			wantCalls: 1,
		},
		{
			name:      "an ordinary question about the user's data leaves nothing behind",
			user:      realQuestion,
			assistant: realAnswer,
			reply:     `[]`,
			want:      nil,
			wantCalls: 1,
		},
		{
			name:      "a greeting never reaches the model",
			user:      "hey",
			assistant: "Hello! What can I help you with?",
			reply:     `[{"type":"semantic","content":"The user says hey.","confidence":0.9}]`,
			want:      nil,
			wantCalls: 0,
		},
		{
			name:      "a fact the model is unsure it read is not written down",
			user:      realQuestion,
			assistant: realAnswer,
			reply: `[
				{"type":"semantic","content":"The user might be a second-year student.","importance":0.5,"confidence":0.2},
				{"type":"goal","content":"The user is working towards finishing the operating systems course.","importance":0.7,"confidence":0.85}
			]`,
			want: []Candidate{{
				Type: TypeGoal, Content: "The user is working towards finishing the operating systems course.",
				Importance: 0.7, Confidence: 0.85,
			}},
			wantCalls: 1,
		},
		{
			name:      "a model that will not stop is capped",
			user:      realQuestion,
			assistant: realAnswer,
			reply: `[
				{"type":"semantic","content":"The user knows one thing.","importance":0.9,"confidence":0.9},
				{"type":"semantic","content":"The user knows two things.","importance":0.9,"confidence":0.9},
				{"type":"semantic","content":"The user knows three things.","importance":0.9,"confidence":0.9},
				{"type":"semantic","content":"The user knows four things.","importance":0.9,"confidence":0.9}
			]`,
			want: []Candidate{
				{Type: TypeSemantic, Content: "The user knows one thing.", Importance: 0.9, Confidence: 0.9},
				{Type: TypeSemantic, Content: "The user knows two things.", Importance: 0.9, Confidence: 0.9},
				{Type: TypeSemantic, Content: "The user knows three things.", Importance: 0.9, Confidence: 0.9},
			},
			wantCalls: 1,
		},
		{
			name:      "a fact the model rated as not worth keeping is not kept",
			user:      realQuestion,
			assistant: realAnswer,
			reply: `[
				{"type":"episodic","content":"The user asked about their scheduler assignment.","importance":0.2,"confidence":1.0},
				{"type":"goal","content":"The user is working towards finishing the operating systems course.","importance":0.7,"confidence":0.85}
			]`,
			want: []Candidate{{
				Type: TypeGoal, Content: "The user is working towards finishing the operating systems course.",
				Importance: 0.7, Confidence: 0.85,
			}},
			wantCalls: 1,
		},
		{
			name:      "a sentence copied out of the retrieved context is not a fact about the user",
			user:      realQuestion,
			assistant: realAnswer,
			reply: `[
				{"type":"semantic","content":"The aurora borealis appeared over the tundra shortly after midnight.","importance":0.9,"confidence":1.0},
				{"type":"project","content":"The user is working on a scheduler assignment for an operating systems course.","importance":0.8,"confidence":0.9}
			]`,
			want: []Candidate{{
				Type: TypeProject, Content: "The user is working on a scheduler assignment for an operating systems course.",
				Importance: 0.8, Confidence: 0.9,
			}},
			wantCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, jsonProvider(tc.reply))

			stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, tc.user, tc.assistant)
			if err != nil {
				t.Fatalf("ExtractFromTurn: %v", err)
			}
			if got := len(h.provider.Calls()); got != tc.wantCalls {
				t.Fatalf("the model was called %d times, want %d", got, tc.wantCalls)
			}
			if len(stored) != len(tc.want) {
				t.Fatalf("stored %d memories, want %d: %+v", len(stored), len(tc.want), stored)
			}
			for i, want := range tc.want {
				got := stored[i]
				switch {
				case got.Type != want.Type:
					t.Fatalf("memory %d type = %q, want %q", i, got.Type, want.Type)
				case got.Content != want.Content:
					t.Fatalf("memory %d content = %q, want %q", i, got.Content, want.Content)
				case got.Importance != want.Importance:
					t.Fatalf("memory %d importance = %v, want %v", i, got.Importance, want.Importance)
				case got.Confidence != want.Confidence:
					t.Fatalf("memory %d confidence = %v, want %v", i, got.Confidence, want.Confidence)
				}
				// Provenance: every fact says which conversation it came from.
				if got.SourceConversationID == nil || *got.SourceConversationID != h.conv {
					t.Fatalf("memory %d source = %v, want the conversation it was extracted from", i, got.SourceConversationID)
				}
				if got.UserID != h.user {
					t.Fatalf("memory %d belongs to %v, want the caller", i, got.UserID)
				}
			}
			// What was stored is what is retrievable: the embedding written is
			// the embedding of the fact, not of the exchange.
			for i, w := range h.store.writes() {
				if w.Content != tc.want[i].Content {
					t.Fatalf("write %d stored %q", i, w.Content)
				}
				if len(w.Embedding) != fakeDimensions {
					t.Fatalf("write %d has a %d-wide embedding", i, len(w.Embedding))
				}
			}
		})
	}
}

// The extraction call is a classifier, not a conversation: a temperature low
// enough that the same exchange does not become two different facts on two
// runs, and a cap on a reply that should be three sentences.
//
// JSON mode is *off* unless asked for. That is the measured default -- see
// Options.JSONMode -- and the assertion is here so turning it on again is a
// deliberate act rather than a merge.
func TestExtractionIsRunAsAClassifier(t *testing.T) {
	h := newHarness(t, jsonProvider(`[]`))
	if _, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}
	calls := h.provider.Calls()
	if len(calls) != 1 {
		t.Fatalf("the model was called %d times, want 1", len(calls))
	}
	if calls[0].Options.Format != "" {
		t.Fatalf("Format = %q, want it unset by default", calls[0].Options.Format)
	}
	if calls[0].Options.Temperature != DefaultExtractionTemperature {
		t.Fatalf("Temperature = %v, want %v", calls[0].Options.Temperature, DefaultExtractionTemperature)
	}
	if calls[0].Options.MaxTokens != DefaultExtractionMaxTokens {
		t.Fatalf("MaxTokens = %v, want %v", calls[0].Options.MaxTokens, DefaultExtractionMaxTokens)
	}

	// And it is sent when an operator asks for it, so the switch is wired
	// rather than merely declared.
	h.svc.opts.JSONMode = true
	if _, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}
	calls = h.provider.Calls()
	if calls[len(calls)-1].Options.Format != ai.FormatJSON {
		t.Fatalf("Format = %q with JSONMode on, want %q", calls[len(calls)-1].Options.Format, ai.FormatJSON)
	}
}

// The same fact stated again in a later conversation does not become a second
// row. This is what keeps a preference the user mentions every week from
// filling the retrieval budget with copies of itself.
func TestAKnownFactIsNotStoredTwice(t *testing.T) {
	const fact = "The user prefers studying in the morning before class."
	h := newHarness(t, jsonProvider(`[{"type":"preference","content":"`+fact+`","confidence":0.9}]`))
	h.store.seed(h.user, TypePreference, fact)

	stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("stored %d memories, want the known fact skipped: %+v", len(stored), stored)
	}
	if n := len(h.store.all(h.user)); n != 1 {
		t.Fatalf("the user has %d memories, want the original one only", n)
	}
}

// Another user's identical memory is not a duplicate of this user's: the
// duplicate check is scoped like everything else.
func TestTheDuplicateCheckIsScopedToTheOwner(t *testing.T) {
	const fact = "The user prefers studying in the morning before class."
	h := newHarness(t, jsonProvider(`[{"type":"preference","content":"`+fact+`","confidence":0.9}]`))
	stranger := uuid.New()
	h.store.seed(stranger, TypePreference, fact)

	stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d memories, want 1 -- another user's memory is not this user's", len(stored))
	}
	if n := len(h.store.all(stranger)); n != 1 {
		t.Fatalf("the other user now has %d memories", n)
	}
}

// --- failure modes ----------------------------------------------------------

// Each of these returns an error and writes nothing. The chat orchestrator
// swallows the error -- see chat.Service.remember -- so what matters here is
// that nothing half-formed is left behind.
func TestExtractionFailuresStoreNothing(t *testing.T) {
	t.Run("the model refuses the call", func(t *testing.T) {
		h := newHarness(t, &ai.Mock{Err: ai.ErrUnavailable})
		stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
		if !errors.Is(err, ErrExtraction) || !errors.Is(err, ai.ErrUnavailable) {
			t.Fatalf("err = %v, want an extraction error wrapping the outage", err)
		}
		if len(stored) != 0 || len(h.store.all(h.user)) != 0 {
			t.Fatalf("a refused call stored %+v", h.store.all(h.user))
		}
	})

	t.Run("the stream is cut off partway", func(t *testing.T) {
		h := newHarness(t, &ai.Mock{
			Reply:     `[{"type":"semantic","content":"The user knows Go.","confidence":0.9}]`,
			StreamErr: ai.ErrUnavailable,
		})
		_, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
		if !errors.Is(err, ErrExtraction) {
			t.Fatalf("err = %v, want an extraction error", err)
		}
		if n := len(h.store.all(h.user)); n != 0 {
			t.Fatalf("a truncated extraction stored %d memories", n)
		}
	})

	t.Run("the embedding service is down", func(t *testing.T) {
		h := newHarness(t, jsonProvider(`[{"type":"semantic","content":"The user knows Go.","confidence":0.9}]`))
		h.embedder.err = embeddings.ErrUnavailable
		_, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
		if !errors.Is(err, ErrEmbedding) || !errors.Is(err, embeddings.ErrUnavailable) {
			t.Fatalf("err = %v, want an embedding error wrapping the outage", err)
		}
		if n := len(h.store.all(h.user)); n != 0 {
			t.Fatalf("a memory was stored without an embedding: %+v", h.store.all(h.user))
		}
	})

	t.Run("the write fails", func(t *testing.T) {
		h := newHarness(t, jsonProvider(`[{"type":"semantic","content":"The user knows Go.","confidence":0.9}]`))
		h.store.createErr = errors.New("connection reset")
		stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
		if err == nil {
			t.Fatal("a failed write was reported as success")
		}
		if len(stored) != 0 {
			t.Fatalf("stored = %+v, want nothing", stored)
		}
	})

	t.Run("the model answers with prose", func(t *testing.T) {
		h := newHarness(t, jsonProvider("I'm sorry, I can't help with that."))
		stored, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv, realQuestion, realAnswer)
		// An unparseable reply is not an error: the model was asked whether
		// there was anything to remember and did not say there was.
		if err != nil {
			t.Fatalf("err = %v, want an unparseable reply to be treated as nothing found", err)
		}
		if len(stored) != 0 {
			t.Fatalf("stored %+v from prose", stored)
		}
	})
}

// --- retrieval --------------------------------------------------------------

func TestSearchAppliesTheFloorAndTheOwner(t *testing.T) {
	h := newHarness(t, nil)
	h.store.seed(h.user, TypePreference, "The user prefers studying in the morning before class")
	h.store.seed(uuid.New(), TypePreference, "The user prefers studying in the morning before class")

	found, err := h.svc.Search(context.Background(), h.user, SearchQuery{
		Query: "studying in the morning before class", MinSimilarity: DefaultMinSimilarity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d memories, want only the caller's: %+v", len(found), found)
	}
	if found[0].Similarity < DefaultMinSimilarity {
		t.Fatalf("similarity = %v, below the floor it was searched with", found[0].Similarity)
	}

	// An unrelated question retrieves nothing rather than the nearest fact
	// however far away it is.
	found, err = h.svc.Search(context.Background(), h.user, SearchQuery{
		Query: "quarterly revenue forecast spreadsheet", MinSimilarity: DefaultMinSimilarity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("an unrelated question retrieved %+v", found)
	}
}

func TestSearchRejectsAnEmptyQuery(t *testing.T) {
	h := newHarness(t, nil)
	var verrs validate.Errors
	if _, err := h.svc.Search(context.Background(), h.user, SearchQuery{Query: "  "}); !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if len(h.embedder.embedded()) != 0 {
		t.Fatal("an invalid query was sent to the embedder")
	}
}

func TestADisabledMemoryIsNeverRetrieved(t *testing.T) {
	h := newHarness(t, nil)
	const fact = "The user prefers studying in the morning before class"
	m := h.store.seed(h.user, TypePreference, fact)

	off := false
	if _, err := h.svc.Update(context.Background(), h.user, m.ID, UpdateInput{Enabled: optional.Of(off)}); err != nil {
		t.Fatal(err)
	}

	found, err := h.svc.Search(context.Background(), h.user, SearchQuery{Query: fact})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("a disabled memory was retrieved: %+v", found)
	}
	// Disabled is not deleted: it is still listed, and can be switched back on.
	listed, err := h.svc.List(context.Background(), h.user, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Enabled {
		t.Fatalf("list = %+v, want the memory present and disabled", listed)
	}
}

// --- management -------------------------------------------------------------

// Editing the text has to move the vector with it, or the memory stays
// retrievable by what it used to say and is invisible to what it now says.
func TestEditingContentReEmbedsAndTogglingDoesNot(t *testing.T) {
	h := newHarness(t, nil)
	m := h.store.seed(h.user, TypeSemantic, "The user knows Go")

	corrected := "Rust is the language the user is learning now"
	updated, err := h.svc.Update(context.Background(), h.user, m.ID, UpdateInput{Content: optional.Of(corrected)})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Content != corrected {
		t.Fatalf("content = %q, want %q", updated.Content, corrected)
	}
	if calls := h.embedder.embedded(); len(calls) != 1 || calls[0][0] != corrected {
		t.Fatalf("embedder saw %v, want exactly the corrected text", calls)
	}

	// The old wording no longer finds it; the new one does. Both queries are
	// the sentences themselves, so the only thing that can move the score is
	// which text the stored vector was built from.
	found, err := h.svc.Search(context.Background(), h.user, SearchQuery{Query: corrected, MinSimilarity: DefaultMinSimilarity})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("the edited memory is not retrievable by its new text: %+v", found)
	}
	found, err = h.svc.Search(context.Background(), h.user, SearchQuery{Query: "The user knows Go", MinSimilarity: DefaultMinSimilarity})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("the edited memory is still retrievable by its old text: %+v", found)
	}

	// A toggle changes no text, so it costs no embedding call.
	before := len(h.embedder.embedded())
	on := true
	if _, err := h.svc.Update(context.Background(), h.user, m.ID, UpdateInput{Enabled: optional.Of(on)}); err != nil {
		t.Fatal(err)
	}
	// The two Search calls above each embedded their query; only the write
	// matters here.
	if after := len(h.embedder.embedded()); after != before {
		t.Fatalf("toggling `enabled` re-embedded the memory (%d -> %d calls)", before, after)
	}
}

func TestUpdateRejectsAnEmptyOrClearedContent(t *testing.T) {
	h := newHarness(t, nil)
	m := h.store.seed(h.user, TypeSemantic, "The user knows Go")

	for _, tc := range []struct {
		name string
		in   UpdateInput
	}{
		{"blank", UpdateInput{Content: optional.Of("   ")}},
		{"null", UpdateInput{Content: optional.Null[string]()}},
		{"null enabled", UpdateInput{Enabled: optional.Null[bool]()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var verrs validate.Errors
			if _, err := h.svc.Update(context.Background(), h.user, m.ID, tc.in); !errors.As(err, &verrs) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
	if got := h.store.all(h.user)[0].Content; got != "The user knows Go" {
		t.Fatalf("a rejected patch changed the memory to %q", got)
	}
}

// "Forget everything about me" is irreversible, so it is refused unless it was
// asked for on purpose -- at the service, not only at the handler.
func TestClearRequiresConfirmation(t *testing.T) {
	h := newHarness(t, nil)
	h.store.seed(h.user, TypeSemantic, "The user knows Go")
	h.store.seed(h.user, TypePreference, "The user prefers mornings")

	var verrs validate.Errors
	n, err := h.svc.Clear(context.Background(), h.user, false)
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a validation error", err)
	}
	if verrs[0].Field != "confirm" {
		t.Fatalf("the error names %q, want confirm", verrs[0].Field)
	}
	if n != 0 || len(h.store.all(h.user)) != 2 {
		t.Fatalf("an unconfirmed clear deleted %d memories", 2-len(h.store.all(h.user)))
	}

	n, err = h.svc.Clear(context.Background(), h.user, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cleared %d memories, want 2", n)
	}
	if left := h.store.all(h.user); len(left) != 0 {
		t.Fatalf("%d memories survived the clear", len(left))
	}
}

func TestClearOnlyTouchesTheCaller(t *testing.T) {
	h := newHarness(t, nil)
	stranger := uuid.New()
	h.store.seed(h.user, TypeSemantic, "The user knows Go")
	h.store.seed(stranger, TypeSemantic, "The user knows Rust")

	if _, err := h.svc.Clear(context.Background(), h.user, true); err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.all(stranger)); n != 1 {
		t.Fatalf("the other user has %d memories left, want theirs untouched", n)
	}
}

func TestListRejectsAnUnknownSortOrType(t *testing.T) {
	h := newHarness(t, nil)
	for _, f := range []Filter{{Sort: "importance; DROP TABLE memories"}, {Type: "vibes"}, {Limit: -1}, {Offset: -1}} {
		var verrs validate.Errors
		if _, err := h.svc.List(context.Background(), h.user, f); !errors.As(err, &verrs) {
			t.Fatalf("filter %+v: err = %v, want a validation error", f, err)
		}
	}
}

// The authenticated user id, and nothing from the request, is what reaches the
// store on every call. This is the same property the Phase 2/3/4 modules pin,
// and the reason the fakes key by owner.
func TestEveryStoreCallCarriesTheCallersID(t *testing.T) {
	h := newHarness(t, jsonProvider(`[{"type":"semantic","content":"The user knows Go.","confidence":0.9}]`))
	ctx := context.Background()

	if _, err := h.svc.ExtractFromTurn(ctx, h.user, h.conv, realQuestion, realAnswer); err != nil {
		t.Fatal(err)
	}
	m := h.store.all(h.user)[0]
	if _, err := h.svc.Get(ctx, h.user, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.List(ctx, h.user, Filter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Update(ctx, h.user, m.ID, UpdateInput{Content: optional.Of("The user knows Rust.")}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Search(ctx, h.user, SearchQuery{Query: "what do I know?"}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Delete(ctx, h.user, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Clear(ctx, h.user, true); err != nil {
		t.Fatal(err)
	}

	seen := h.store.callersSeen()
	if len(seen) == 0 {
		t.Fatal("the store was never called")
	}
	for i, id := range seen {
		if id != h.user {
			t.Fatalf("store call %d was made for %v, want the caller %v", i, id, h.user)
		}
	}
}

// The exchange the extractor is given is the one that just happened, quoted --
// not the retrieved context, and not the system prompt the assistant answered
// under.
func TestTheExtractorSeesOnlyTheExchange(t *testing.T) {
	h := newHarness(t, jsonProvider(`[]`))
	if _, err := h.svc.ExtractFromTurn(context.Background(), h.user, h.conv,
		"I am learning Rust for my systems course this term, on top of the Go I already use at work.",
		"That fits the course well, and the ownership model will feel familiar after Go's escape analysis."); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{"I am learning Rust", "That fits the course well"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "cite it with its label") {
		t.Fatalf("the assistant's grounding contract leaked into the extraction prompt:\n%s", prompt)
	}
}
