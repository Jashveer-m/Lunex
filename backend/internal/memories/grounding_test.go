package memories

import (
	"context"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// A change that has not happened is not a fact about the user, however the
// model phrases it -- and a fact that shares nothing with the change is kept.
func TestRestatesUnconfirmed(t *testing.T) {
	changes := []string{"Book a flight to Delhi"}
	for content, want := range map[string]bool{
		"The user booked a flight to Delhi.":          true,
		"The user is planning a trip to Delhi.":       true,
		"The user has a flight booked.":               true,
		"The user is vegetarian.":                     false,
		"The user prefers working in the morning.":    false,
		"The user's thesis is on distributed systems": false,
	} {
		if got := RestatesUnconfirmed(content, changes); got != want {
			t.Errorf("RestatesUnconfirmed(%q) = %v, want %v", content, got, want)
		}
	}
	if RestatesUnconfirmed("The user booked a flight to Delhi.", nil) {
		t.Error("with nothing unconfirmed, nothing restates it")
	}
}

// The measured failure: a document's content phrased as something the user
// knows. What the user did say is never held against a fact.
func TestRestatesRetrieved(t *testing.T) {
	passages := []string{"Seedling log, spring. Tap water stunted last year's seedlings; " +
		"switch to collected rainwater and keep the greenhouse above 12 degrees at night."}
	for _, tc := range []struct {
		content, user string
		want          bool
	}{
		{"The user knows that tap water stunted last year's seedlings.",
			"What does my garden log say about watering the seedlings?", true},
		{"The user keeps the greenhouse above 12 degrees at night.",
			"What temperature should the greenhouse be?", true},
		// Measured against llama3.2:3b: framing words the model added must not
		// dilute the document's share.
		{"The user prefers using collected rainwater for seedlings.",
			"What does my garden log say about watering the seedlings?", true},
		{"The user's garden log recommends using collected rainwater for seedlings.",
			"What does my garden log say about watering the seedlings?", true},
		// Said by the user, so theirs -- even though the document agrees.
		{"The user collects rainwater for the greenhouse.",
			"I collect rainwater for the greenhouse now. What does my log say about seedlings?", false},
		{"The user is vegetarian.", "I'm vegetarian, does my log mention any vegetables?", false},
	} {
		if got := RestatesRetrieved(tc.content, tc.user, passages); got != tc.want {
			t.Errorf("RestatesRetrieved(%q | %q) = %v, want %v", tc.content, tc.user, got, tc.want)
		}
	}
	if RestatesRetrieved("The user knows that tap water stunted last year's seedlings.", "hi", nil) {
		t.Error("with nothing retrieved, nothing restates it")
	}
}

// Through the pipeline: of three confident, important, "The user ..." facts --
// each of which passes AboutTheUser -- only the one the user actually stated is
// stored. The prompt names the unconfirmed change as not a fact.
func TestExtractStoresOnlyConfirmedFactsTheUserStated(t *testing.T) {
	h := newHarness(t, jsonProvider(`[
		{"type":"episodic","content":"The user booked a flight to Delhi.","importance":0.8,"confidence":0.9},
		{"type":"semantic","content":"The user knows that tap water stunted last year's seedlings.","importance":0.7,"confidence":0.9},
		{"type":"preference","content":"The user is vegetarian.","importance":0.8,"confidence":0.95}
	]`))
	turn := Turn{
		UserMessage:      "I'm vegetarian, by the way. Add a task to book a flight to Delhi, and what does my garden log say about the seedlings?",
		AssistantMessage: "I have prepared a task to book a flight to Delhi; approve it to create it. Your log says tap water stunted last year's seedlings [S1].",
		Unconfirmed:      []string{"Book a flight to Delhi"},
		Retrieved:        []string{"Tap water stunted last year's seedlings; switch to collected rainwater."},
	}
	stored, err := h.svc.Extract(context.Background(), h.user, h.conv, turn)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Content != "The user is vegetarian." {
		t.Fatalf("stored %+v, want only the vegetarian preference", stored)
	}

	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{"NOT FACTS", "- Book a flight to Delhi", "have NOT happened"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// A turn with nothing unconfirmed gets the prompt it always had: no empty
// NOT FACTS section for the model to puzzle over.
func TestAPlainTurnHasNoNotFactsSection(t *testing.T) {
	if p := ai.PromptText(extractionPromptFor(Turn{UserMessage: "I play the cello", AssistantMessage: "Nice."})); strings.Contains(p, "NOT FACTS") {
		t.Fatalf("prompt = %s", p)
	}
}

// Non-work preferences are asked for by name, in the type definition as well as
// the instructions: the old prompt defined a preference as "how they like to
// work", and "I'm vegetarian" came back as [].
func TestThePromptAsksForPreferencesBeyondWork(t *testing.T) {
	p := ai.PromptText(ExtractionPrompt("x", "y"))
	for _, want := range []string{"not only their work", "food and diet", `"preference" (something they like, dislike, prefer or avoid -- in food, lifestyle, hobbies or work)`} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt is missing %q", want)
		}
	}
	if strings.Contains(p, `"preference" (how they like to work)`) {
		t.Fatal("the prompt still defines a preference as a way of working")
	}
}

// A fact must share a content word with what the user said. Both measured
// leaks share none: one from the assistant's suggestion, one from the prompt's
// own worked example.
func TestSaidByTheUser(t *testing.T) {
	for _, tc := range []struct {
		content, user string
		want          bool
	}{
		{"The user is a vegetarian.", "I'm vegetarian, so keep that in mind for meals.", true},
		{"The user does not drink coffee.", "I don't drink coffee at all.", true},
		{"The user enjoys hiking in the hills on weekends.", "I love hiking in the hills on weekends.", true},
		{"The user prefers herbal tea in the afternoons.", "I don't drink coffee at all, it keeps me up.", false},
		{"The user's thesis topic is distributed consensus.", "I'm vegetarian, so keep that in mind for meals.", false},
	} {
		if got := SaidByTheUser(tc.content, tc.user); got != tc.want {
			t.Errorf("SaidByTheUser(%q | %q) = %v, want %v", tc.content, tc.user, got, tc.want)
		}
	}
}
