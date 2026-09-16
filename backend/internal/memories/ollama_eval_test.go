package memories

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// memoryEvalCases are the extraction failures the hardening pass fixed, as
// exchanges a real model reads. want is a word every kept fact set must
// mention (case-insensitive) -- "" means nothing may be kept -- and banned is
// words no kept fact may contain.
var memoryEvalCases = []struct {
	name   string
	turn   Turn
	want   string
	banned []string
}{
	// Non-work preferences: the old prompt defined a preference as "how they
	// like to work", and these came back [].
	{name: "vegetarian", want: "vegetarian", turn: Turn{
		UserMessage:      "I'm vegetarian, so please keep that in mind whenever you suggest meals or recipes for me.",
		AssistantMessage: "Understood. I will only suggest vegetarian meals and recipes from now on."}},
	// The same statement as the answering model actually replied to it in a
	// live run: a reply saying nothing was found made the extractor find
	// nothing too.
	{name: "vegetarian, nothing found", want: "vegetarian", banned: []string{"thesis", "lunch"}, turn: Turn{
		UserMessage: "I'm vegetarian, so please keep that in mind whenever you suggest meals for me.",
		AssistantMessage: "I could not find anything about your dietary preferences or restrictions in your documents, tasks, goals, or notes.\n\n" +
			"However, I can provide general suggestions for vegetarian meal ideas. Would you like some options?"}},
	{name: "coffee", want: "coffee", banned: []string{"tea"}, turn: Turn{
		UserMessage:      "Just so you know, I don't drink coffee at all, it keeps me up for the whole night.",
		AssistantMessage: "Got it -- no coffee. Herbal tea might be a good alternative in the afternoons."}},
	{name: "hiking", want: "hik", turn: Turn{
		UserMessage:      "I love hiking in the hills on weekends, it's the one thing that really clears my head.",
		AssistantMessage: "That sounds like a great way to recharge. Weekends are free in your calendar."}},
	{name: "work preference", want: "meeting", turn: Turn{
		UserMessage:      "At work I much prefer written async updates over meetings, meetings drain my energy.",
		AssistantMessage: "Noted. Async written updates it is."}},
	// A proposal, rejected or pending, is not a fact.
	{name: "a proposed flight", want: "", banned: []string{"delhi", "flight"}, turn: Turn{
		UserMessage:      "Add a task to book a flight to Delhi for the conference next month.",
		AssistantMessage: `I have prepared a change to create a task "Book a flight to Delhi". It will only happen once you approve it with the Approve button on the card shown with this reply.`,
		Unconfirmed:      []string{"Book a flight to Delhi for the conference"}}},
	// A document's content is not something the user knows.
	{name: "a document restated", want: "", banned: []string{"tap", "stunt", "rainwater", "12 degrees"}, turn: Turn{
		UserMessage: "What does my garden log say about watering the seedlings?",
		AssistantMessage: "According to your garden log [S1], tap water stunted last year's seedlings, " +
			"so the log recommends switching to collected rainwater and keeping the greenhouse above 12 degrees at night.",
		Retrieved: []string{"Garden log, spring. Tap water stunted last year's seedlings; switch to collected " +
			"rainwater and keep the greenhouse above 12 degrees at night. The user should repot the tomatoes in April."}}},
}

// TestMemoryExtractionAgainstOllama runs the production prompt, parser and
// filters against a real model. Opt-in, like the routing eval:
//
//	LUNEX_MEMORY_EVAL=1 go test ./internal/memories -run Ollama -v -timeout 30m
//
// A model is not deterministic, so each case runs LUNEX_MEMORY_EVAL_RUNS times
// (default 3) and every run is logged. It fails on a banned word ever being
// kept -- the property that matters -- and on a wanted fact missed in most runs.
func TestMemoryExtractionAgainstOllama(t *testing.T) {
	if os.Getenv("LUNEX_MEMORY_EVAL") == "" {
		t.Skip("LUNEX_MEMORY_EVAL not set; skipping the memory extraction eval against Ollama")
	}
	runs := 3
	if v := os.Getenv("LUNEX_MEMORY_EVAL_RUNS"); v != "" {
		if _, err := fmt.Sscan(v, &runs); err != nil || runs < 1 {
			t.Fatalf("LUNEX_MEMORY_EVAL_RUNS=%q", v)
		}
	}
	provider := ai.NewOllama(os.Getenv("OLLAMA_BASE_URL"), os.Getenv("CHAT_MODEL"), 10*time.Minute)
	svc := NewService(Deps{Provider: provider, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options: Options{Timeout: 10 * time.Minute}})

	only := os.Getenv("LUNEX_MEMORY_EVAL_ONLY")
	for _, tc := range memoryEvalCases {
		if only != "" && !strings.Contains(tc.name, only) {
			continue
		}
		hits := 0
		for i := 0; i < runs; i++ {
			kept, err := svc.propose(context.Background(), tc.turn)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			var facts []string
			for _, c := range kept {
				facts = append(facts, c.Type+": "+c.Content)
			}
			all := strings.ToLower(strings.Join(facts, " | "))
			t.Logf("%-20s run %d: %v", tc.name, i+1, facts)
			for _, b := range tc.banned {
				if strings.Contains(all, b) {
					t.Errorf("%s: kept a fact containing %q: %v", tc.name, b, facts)
				}
			}
			if tc.want != "" && strings.Contains(all, tc.want) {
				hits++
			}
		}
		if tc.want != "" && hits*2 <= runs {
			t.Errorf("%s: a fact mentioning %q was kept in %d of %d runs", tc.name, tc.want, hits, runs)
		}
	}
}
