package study

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
)

// The parser takes every shape a model actually replies in, and nothing it
// cannot read becomes a card.
func TestParseFlashcardsAcceptsTheShapesAModelWrites(t *testing.T) {
	for name, reply := range map[string]string{
		"a bare array": `[{"front":"Q1?","back":"A1."},{"front":"Q2?","back":"A2."}]`,
		"a fenced array": "```json\n" +
			`[{"front":"Q1?","back":"A1."},{"front":"Q2?","back":"A2."}]` + "\n```",
		"prose in front of it": `Here are the flashcards:
[{"front":"Q1?","back":"A1."},{"front":"Q2?","back":"A2."}]
Let me know if you want more.`,
		"a wrapper object":  `{"flashcards":[{"front":"Q1?","back":"A1."},{"front":"Q2?","back":"A2."}]}`,
		"question / answer": `[{"question":"Q1?","answer":"A1."},{"question":"Q2?","answer":"A2."}]`,
		"term / definition": `[{"term":"Q1?","definition":"A1."},{"term":"Q2?","definition":"A2."}]`,
		"the line fallback": "Q1? | A1.\nQ2? | A2.",
		"a numbered line fallback": `1. Q1? | A1.
2. Q2? | A2.`,
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseFlashcards(reply, 8)
			if len(got) != 2 {
				t.Fatalf("parsed %d cards, want 2: %+v", len(got), got)
			}
			if got[0].Front != "Q1?" || got[0].Back != "A1." {
				t.Fatalf("first card = %+v", got[0])
			}
		})
	}
}

func TestParseFlashcardsReadsASingleObject(t *testing.T) {
	got := ParseFlashcards(`{"front":"Q?","back":"A."}`, 8)
	if len(got) != 1 || got[0].Front != "Q?" {
		t.Fatalf("cards = %+v", got)
	}
}

// A card missing a side is dropped rather than completed. A question with
// nothing on the back is not an incomplete card, it is a card that would be
// shown to somebody trying to learn with nothing to learn.
func TestParseFlashcardsDropsHalfCards(t *testing.T) {
	got := ParseFlashcards(`[
	  {"front":"Q1?"},
	  {"back":"A2."},
	  {"front":"  ","back":"A3."},
	  {"front":"Q4?","back":"A4."}
	]`, 8)
	if len(got) != 1 || got[0].Front != "Q4?" {
		t.Fatalf("cards = %+v, want only the complete one", got)
	}
}

func TestParseFlashcardsKeepsOneCardPerQuestion(t *testing.T) {
	got := ParseFlashcards(`[
	  {"front":"What is it?","back":"A."},
	  {"front":"what is IT?","back":"A different answer."}
	]`, 8)
	if len(got) != 1 {
		t.Fatalf("parsed %d cards, want the repeat collapsed: %+v", len(got), got)
	}
}

// Multi-line strings are what a model writes when it is copying out of a
// document it has just read. They are the same card as the tidy version to a
// person and a different string to the duplicate check, so they are collapsed.
func TestParseFlashcardsCollapsesWhitespace(t *testing.T) {
	got := ParseFlashcards("[{\"front\":\"What  is\\n  the answer?\",\"back\":\"It\\tis\\nthis.\"}]", 8)
	if len(got) != 1 || got[0].Front != "What is the answer?" || got[0].Back != "It is this." {
		t.Fatalf("cards = %+v", got)
	}
}

// The cap is enforced here rather than trusted to the prompt, because a prompt
// is a request and a cap is a guarantee.
func TestParseFlashcardsHonoursTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < MaxCardsPerBatch+10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"front":"Q` + string(rune('a'+i%26)) + `?","back":"A."}`)
	}
	b.WriteString("]")

	if got := ParseFlashcards(b.String(), 3); len(got) != 3 {
		t.Fatalf("a limit of 3 produced %d cards", len(got))
	}
	if got := ParseFlashcards(b.String(), 0); len(got) > MaxCardsPerBatch {
		t.Fatalf("no limit produced %d cards, want at most %d", len(got), MaxCardsPerBatch)
	}
}

func TestParseFlashcardsYieldsNothingForARefusal(t *testing.T) {
	for name, reply := range map[string]string{
		"empty":             "",
		"an empty array":    "[]",
		"prose":             "I could not find anything in the passages worth making cards from.",
		"a prose paragraph": "The document describes an aurora. It also mentions a generator.",
		"nonsense":          "{{{",
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseFlashcards(reply, 8); got != nil {
				t.Fatalf("parsed %+v", got)
			}
		})
	}
}

// The text a card is checked against is exactly the text the model was shown,
// which is what PromptPassages being used for both makes true.
func TestPromptPassagesBoundsWhatTheModelSees(t *testing.T) {
	var many []documents.Passage
	for i := 0; i < PassagesPerGeneration+5; i++ {
		many = append(many, documents.Passage{
			ChunkID: uuid.New(), ChunkIndex: i, Content: "passage number " + string(rune('a'+i)),
		})
	}
	if got := PromptPassages(many); len(got) != PassagesPerGeneration {
		t.Fatalf("shown %d passages, want %d", len(got), PassagesPerGeneration)
	}

	huge := []documents.Passage{{Content: strings.Repeat("x", MaxGroundingChars*2)}}
	shown := PromptPassages(huge)
	if len(shown) != 1 || len([]rune(shown[0].Content)) > MaxGroundingChars+1 {
		t.Fatalf("a huge passage was shown at %d characters", len([]rune(shown[0].Content)))
	}
}

func TestGenerationPromptNumbersThePassagesAndNamesTheFile(t *testing.T) {
	msgs := GenerationPrompt([]documents.Passage{
		{ChunkIndex: 0, Content: auroraPassage},
		{ChunkIndex: 1, Content: generatorPassage},
	}, "field-notes.txt", "", 5)
	if len(msgs) != 2 {
		t.Fatalf("prompt has %d messages", len(msgs))
	}
	body := msgs[1].Content
	for _, want := range []string{"field-notes.txt", "[1] The aurora", "[2] The generator",
		"Write up to 5 flashcards"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, body)
		}
	}
}
