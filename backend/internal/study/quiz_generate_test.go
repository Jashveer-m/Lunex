package study

import (
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/documents"
)

// What ParseQuizQuestions has to survive is a model that was asked for four
// named fields and wrote something adjacent. Every shape below was chosen
// because it is what a small model actually writes.

func TestParseQuizQuestionsAcceptsTheShapesAModelWrites(t *testing.T) {
	for name, reply := range map[string]string{
		"a bare array":                  `[{"question":"How long?","options":["Ten","Eleven"],"correct_index":1}]`,
		"a wrapper object":              `{"questions":[{"question":"How long?","options":["Ten","Eleven"],"correct_index":1}]}`,
		"a single object":               `{"question":"How long?","options":["Ten","Eleven"],"correct_index":1}`,
		"a markdown fence":              "```json\n[{\"question\":\"How long?\",\"options\":[\"Ten\",\"Eleven\"],\"correct_index\":1}]\n```",
		"leading prose":                 `Here is the quiz: [{"question":"How long?","options":["Ten","Eleven"],"correct_index":1}]`,
		"the answer as text":            `[{"question":"How long?","options":["Ten","Eleven"],"answer":"Eleven"}]`,
		"the answer as a letter":        `[{"question":"How long?","options":["Ten","Eleven"],"answer":"B"}]`,
		"options labelled by the model": `[{"question":"How long?","options":["A) Ten","B) Eleven"],"correct":"B"}]`,
		"options as an object":          `[{"question":"How long?","options":{"A":"Ten","B":"Eleven"},"correct_answer":"Eleven"}]`,
		"the line fallback":             `How long? | Ten | *Eleven`,
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseQuizQuestions(reply, 5)
			if len(got) != 1 {
				t.Fatalf("parsed %d questions from %s", len(got), reply)
			}
			q := got[0]
			switch {
			case q.Question != "How long?":
				t.Fatalf("question = %q", q.Question)
			case strings.Join(q.Options, "|") != "Ten|Eleven":
				t.Fatalf("options = %v, want the label stripped and the order kept", q.Options)
			case q.CorrectIndex != 1:
				t.Fatalf("correct_index = %d, want 1 (%q)", q.CorrectIndex, q.CorrectAnswer())
			}
		})
	}
}

// The one wrong answer this parser could give. `correct_index` names a
// position and `answer` names a value, and a quiz whose options are themselves
// numbers is where the difference decides which option is marked right.
func TestAnIndexIsAPositionAndAnAnswerIsAValue(t *testing.T) {
	numbered := `[{"question":"How many cells?","options":["2","4","6","8"],"%s":%s}]`
	for name, tc := range map[string]struct {
		field string
		value string
		want  int
	}{
		"correct_index counts options":     {"correct_index", "2", 2},
		"answer_index counts options":      {"answer_index", "3", 3},
		"an answer written out is a value": {"answer", `"2"`, 0},
		"a numeric answer is a position":   {"answer", "2", 2},
	} {
		t.Run(name, func(t *testing.T) {
			got := ParseQuizQuestions(strings.Replace(
				strings.Replace(numbered, "%s", tc.field, 1), "%s", tc.value, 1), 5)
			if len(got) != 1 {
				t.Fatalf("parsed %d questions", len(got))
			}
			if got[0].CorrectIndex != tc.want {
				t.Fatalf("correct_index = %d (%q), want %d",
					got[0].CorrectIndex, got[0].CorrectAnswer(), tc.want)
			}
		})
	}
}

// The answer written out wins over an index that disagrees with it: a model
// writes the option's text correctly far more often than it counts.
func TestTheWrittenAnswerBeatsAnIndexThatDisagrees(t *testing.T) {
	got := ParseQuizQuestions(
		`[{"question":"How long?","options":["Ten","Eleven","Twelve"],"answer":"Twelve","correct_index":0}]`, 5)
	if len(got) != 1 || got[0].CorrectIndex != 2 {
		t.Fatalf("got %+v, want the option the model named", got)
	}
}

// A question whose answer cannot be resolved to exactly one option is dropped,
// not guessed at: a guess is a quiz that marks a right answer wrong.
func TestAQuestionWithNoResolvableAnswerIsDropped(t *testing.T) {
	for name, reply := range map[string]string{
		"no answer at all":          `[{"question":"How long?","options":["Ten","Eleven"]}]`,
		"an index past the end":     `[{"question":"How long?","options":["Ten","Eleven"],"correct_index":7}]`,
		"a negative index":          `[{"question":"How long?","options":["Ten","Eleven"],"correct_index":-1}]`,
		"a letter past the end":     `[{"question":"How long?","options":["Ten","Eleven"],"answer":"D"}]`,
		"text matching no option":   `[{"question":"How long?","options":["Ten","Eleven"],"answer":"Nine"}]`,
		"no options":                `[{"question":"How long?","correct_index":0}]`,
		"one option":                `[{"question":"How long?","options":["Ten"],"correct_index":0}]`,
		"no question":               `[{"options":["Ten","Eleven"],"correct_index":0}]`,
		"a blank option":            `[{"question":"How long?","options":["Ten",""],"correct_index":0}]`,
		"the same option twice":     `[{"question":"How long?","options":["Ten","ten"],"correct_index":0}]`,
		"an unmarked fallback line": `How long? | Ten | Eleven`,
		"two marked fallback lines": `How long? | *Ten | *Eleven`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := ParseQuizQuestions(reply, 5); got != nil {
				t.Fatalf("parsed %+v, want nothing", got)
			}
		})
	}
}

// One fact per question was asked for; the same question twice is the model
// restating rather than a second fact, whatever it marked correct.
func TestParseQuizQuestionsKeepsOneQuestionPerWording(t *testing.T) {
	got := ParseQuizQuestions(`[
		{"question":"How long?","options":["Ten","Eleven"],"correct_index":1},
		{"question":"how long?","options":["Nine","Eleven"],"correct_index":1}]`, 5)
	if len(got) != 1 {
		t.Fatalf("parsed %d questions, want the duplicate dropped", len(got))
	}
}

func TestParseQuizQuestionsHonoursTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < MaxQuestionsPerQuiz+5; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"question":"Question ` + string(rune('a'+i)) +
			`?","options":["Ten","Eleven"],"correct_index":0}`)
	}
	b.WriteString("]")

	if got := ParseQuizQuestions(b.String(), 3); len(got) != 3 {
		t.Fatalf("asked for 3, got %d", len(got))
	}
	// A count the caller did not clamp is still capped here, because a prompt
	// is a request and a cap is a guarantee.
	if got := ParseQuizQuestions(b.String(), 0); len(got) != MaxQuestionsPerQuiz {
		t.Fatalf("with no limit: %d, want %d", len(got), MaxQuestionsPerQuiz)
	}
}

func TestParseQuizQuestionsYieldsNothingForARefusal(t *testing.T) {
	for _, reply := range []string{
		"", "   ", "[]", "I could not find anything in the passages to ask about.",
		"```json\n[]\n```", "{}",
	} {
		if got := ParseQuizQuestions(reply, 5); got != nil {
			t.Fatalf("%q parsed as %+v", reply, got)
		}
	}
}

// The topic is the model's or nothing: a question with no tag gets none rather
// than one derived from its wording, which would give 10c a tag that groups by
// phrasing.
func TestATopicIsTakenOrLeftEmpty(t *testing.T) {
	got := ParseQuizQuestions(`[
		{"question":"How long?","options":["Ten","Eleven"],"correct_index":1,"topic":" changeover  timing "},
		{"question":"How many?","options":["Two","Six"],"correct_index":1}]`, 5)
	if len(got) != 2 {
		t.Fatalf("parsed %d", len(got))
	}
	if got[0].Topic != "changeover timing" {
		t.Fatalf("topic = %q, want it collapsed", got[0].Topic)
	}
	if got[1].Topic != "" {
		t.Fatalf("topic = %q, want none invented", got[1].Topic)
	}
}

func TestQuizGenerationPromptNumbersThePassagesAndNamesTheFile(t *testing.T) {
	msgs := QuizGenerationPrompt([]documents.Passage{
		{ChunkIndex: 0, Content: "first passage"},
		{ChunkIndex: 1, Content: "second passage"},
	}, "handbook.txt", "batteries", 4)
	if len(msgs) != 2 {
		t.Fatalf("prompt has %d messages", len(msgs))
	}
	body := msgs[1].Content
	for _, want := range []string{
		"PASSAGES from handbook.txt", "[1] first passage", "[2] second passage",
		`quizzed on "batteries"`, "up to 4 multiple-choice questions",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, body)
		}
	}
}
