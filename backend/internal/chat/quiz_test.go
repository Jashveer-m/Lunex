package chat

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// What the orchestrator does with Phase 10b. A quiz reaches the model the way
// a study plan does -- as a source that names the existence of content it does
// not carry -- and the rule that goes with it has one thing the flashcard half
// does not: here the withholding is the feature.

func TestTheQuizRuleForbidsSpoilingAQuiz(t *testing.T) {
	h := newHarness(t, &ai.Mock{})
	if _, _, err := h.send(t, "Quiz me on the relay handbook."); err != nil {
		t.Fatal(err)
	}

	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		// what the source is, and what it leaves out
		`A source of type "quiz" is a quiz the user made from one of their documents`,
		"The questions, the options and the answers are NOT in the context",
		// the thing the model must not write, including when it is asked to
		"Never state, quote, summarise or guess what a quiz asks",
		"even if the user asks you to",
		// the improvisation this rule exists to stop
		"never ask the user a question from one or mark an answer",
		"You cannot start, answer or finish an attempt",
		// and the score, repeated as written rather than turned into a
		// percentage
		"never work out a percentage",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the quiz rule is missing %q from the prompt:\n%s", want, prompt)
		}
	}
}

// Like the study and finance rules, it is in every prompt rather than only the
// ones a quiz tool ran for: "quiz me" retrieves nothing when the user has no
// quiz, and that is exactly the turn where a model with no rule in front of it
// improvises one and marks the answers itself.
func TestTheQuizRuleIsInThePromptWithNothingRetrieved(t *testing.T) {
	h := newHarness(t, &ai.Mock{})
	if _, _, err := h.send(t, "Ask me some questions."); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	if !strings.Contains(prompt, "The questions, the options and the answers are NOT in the context") {
		t.Fatalf("the quiz rule is missing from a turn with no quiz tool:\n%s", prompt)
	}
}

// A quiz a read tool found becomes a source the model can cite: what it is on,
// how big it is, and how it has gone.
func TestAQuizBecomesACitableSource(t *testing.T) {
	document, plan, best := "relay-handbook.txt", "Kestrel relay handbook", 4
	quiz := study.Quiz{
		ID: uuid.New(), Title: "Kestrel relay handbook", QuestionCount: 6,
		AttemptCount: 3, BestScore: &best, StudyPlanTitle: &plan, DocumentName: &document,
	}

	sources := toolSources(tools.SearchQuizzes, tools.Result{Quizzes: []study.Quiz{quiz}})
	if len(sources) != 1 {
		t.Fatalf("%d sources, want 1", len(sources))
	}
	s := sources[0]
	switch {
	case s.Type != SourceQuiz:
		t.Fatalf("type = %q, want %q", s.Type, SourceQuiz)
	case s.ID != quiz.ID:
		t.Fatalf("id = %v, want the quiz's", s.ID)
	case s.Tool != tools.SearchQuizzes:
		t.Fatalf("tool = %q", s.Tool)
	}
	// The score is written out against the total, so a model handed it cannot
	// work out a percentage of its own from two separate numbers.
	for _, want := range []string{"6 questions", "3 attempts", "best 4 of 6",
		"under " + plan, "made from " + document} {
		if !strings.Contains(s.Excerpt, want) {
			t.Fatalf("the excerpt does not carry %q: %q", want, s.Excerpt)
		}
	}

	// A quiz nobody has taken says so, rather than reporting a best of zero.
	fresh := toolSources(tools.SearchQuizzes, tools.Result{
		Quizzes: []study.Quiz{{ID: uuid.New(), Title: "New", QuestionCount: 1}},
	})
	for _, want := range []string{"1 question", "not attempted yet"} {
		if !strings.Contains(fresh[0].Excerpt, want) {
			t.Fatalf("excerpt = %q, want %q", fresh[0].Excerpt, want)
		}
	}
	if strings.Contains(fresh[0].Excerpt, "best") {
		t.Fatalf("an unattempted quiz reports a best score: %q", fresh[0].Excerpt)
	}
}

// The questions themselves are never a source, and neither is an attempt. A
// quiz counts as one record however many questions it has.
func TestQuizQuestionsAreNotSources(t *testing.T) {
	result := tools.Result{Quizzes: []study.Quiz{{
		ID: uuid.New(), Title: "Quiz", QuestionCount: 15,
		Questions: []study.Question{{Question: "How long?", Options: []string{"Ten", "Eleven"}, CorrectIndex: 1}},
	}}}
	if got := result.Count(); got != 1 {
		t.Fatalf("a quiz with 15 questions counts as %d records, want 1", got)
	}
	for _, s := range toolSources(tools.GenerateQuiz, result) {
		if s.Type != SourceQuiz {
			t.Fatalf("an unexpected source type reached the model: %q", s.Type)
		}
		for _, forbidden := range []string{"How long?", "Eleven"} {
			if strings.Contains(s.Excerpt, forbidden) {
				t.Fatalf("a quiz source leaked %q: %q", forbidden, s.Excerpt)
			}
		}
	}
}

// A created quiz anchors nothing in the knowledge graph, because a quiz has no
// node -- the plan is the thing worth connecting. A nil anchor is the ordinary
// "nothing to anchor to" the extractor already handles.
func TestACreatedQuizHasNoGraphAnchor(t *testing.T) {
	if anchor := anchorFor(tools.Result{Quizzes: []study.Quiz{{ID: uuid.New(), Title: "Quiz"}}}); anchor != nil {
		t.Fatalf("a quiz anchored %+v", anchor)
	}
}

// The ACTIONS block has to stay a sentence the model can hold in one piece.
//
// Every proposal summary was one line when the wording in act.go was measured,
// and interpolating a generated quiz into the middle of a "what it will do
// (...)" clause put llama3.2:3b back where it started: shown a three-question
// quiz inline, it replied with the instruction itself, beginning "I prepared a
// change that has not been made yet. Reply by telling the user...". So a
// multi-line summary goes after the instruction rather than inside it.
func TestALongProposalSummaryGoesAfterTheInstructionNotInsideIt(t *testing.T) {
	const quiz = `Save a quiz "relay-handbook.txt", 2 questions:
1. What is the voltage of the equalisation charge?
   - 40 volts
   - 58.4 volts (correct)
2. How many cells are in the battery bank?
   - Four cells
   - Six cells (correct)
The option marked (correct) is the one the quiz will mark right.`

	step := toolStep{call: &tools.Call{
		Tool: tools.GenerateQuiz, Permission: tools.Write, Summary: quiz + ".",
	}}
	block := step.actionsBlock(nil)

	body := strings.TrimPrefix(block, "ACTIONS\n\n")
	instruction, _, found := strings.Cut(body, "\n")
	if !found {
		t.Fatalf("the block is one line:\n%s", block)
	}
	// The instruction is intact and says what it always said...
	for _, want := range []string{
		"You prepared a change that has not been made yet",
		"what it will do, and that it will only happen once they press Approve",
		"Never say that it is done",
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("the instruction is missing %q:\n%s", want, instruction)
		}
	}
	// ...and none of the quiz is in it.
	for _, leaked := range []string{"58.4 volts", "1.", "(correct)"} {
		if strings.Contains(instruction, leaked) {
			t.Fatalf("the quiz leaked into the instruction sentence (%q):\n%s", leaked, instruction)
		}
	}
	// The quiz is still there, whole, under its own heading.
	if !strings.Contains(block, "What it will do:\n"+quiz) {
		t.Fatalf("the block does not carry the quiz after the instruction:\n%s", block)
	}
}

// The recap of earlier proposals carries one line per action, whatever the
// summary looks like. The same defect had a second site here: the status was
// glued onto the last line of the quiz, so the block said "The option marked
// (correct) is the one the quiz will mark right: the user approved it and it
// was done" -- and a second entry could not be told from the first one's
// options.
func TestTheRecapOfEarlierProposalsIsOneLinePerAction(t *testing.T) {
	quiz := TurnAction{
		Action: actions.Action{Status: actions.StatusExecuted, ToolName: tools.GenerateQuiz},
		Summary: `Save a quiz "relay-handbook.txt", 2 questions:
1. What is the voltage of the equalisation charge?
   - 40 volts
   - 58.4 volts (correct)
The option marked (correct) is the one the quiz will mark right.`,
	}
	task := TurnAction{
		Action:  actions.Action{Status: actions.StatusProposed, ToolName: tools.CreateTask},
		Summary: `Create a task "Buy milk".`,
	}
	block := toolStep{recent: []TurnAction{quiz, task}}.actionsBlock(nil)

	recap := strings.TrimPrefix(block, "ACTIONS\n\nEarlier in this conversation you proposed:\n")
	lines := strings.Split(recap, "\n")
	if len(lines) != 2 {
		t.Fatalf("the recap is %d lines for 2 actions:\n%s", len(lines), block)
	}
	if !strings.HasPrefix(lines[0], `- Save a quiz "relay-handbook.txt", 2 questions`) ||
		!strings.HasSuffix(lines[0], statusInWords(quiz.Action)) {
		t.Fatalf("the quiz recap line is %q", lines[0])
	}
	// The content is not in the recap: it was shown in full when it was
	// proposed, on the card the user read.
	for _, leaked := range []string{"58.4 volts", "(correct)"} {
		if strings.Contains(block, leaked) {
			t.Fatalf("the recap carries %q:\n%s", leaked, block)
		}
	}
	// And a one-line summary is unchanged by any of it.
	if lines[1] != `- Create a task "Buy milk": `+statusInWords(task.Action) {
		t.Fatalf("the task recap line is %q", lines[1])
	}
}

// A one-line summary keeps the inline form, which is the one that was
// measured: this change must not touch what every other write tool produces.
func TestAShortProposalSummaryStaysInline(t *testing.T) {
	step := toolStep{call: &tools.Call{
		Tool: tools.CreateTask, Permission: tools.Write, Summary: `Create a task "Buy milk".`,
	}}
	block := step.actionsBlock(nil)
	if !strings.Contains(block, `what it will do (Create a task "Buy milk"), and that it will only happen`) {
		t.Fatalf("a one-line summary was moved out of the sentence:\n%s", block)
	}
	if strings.Contains(block, "What it will do:") {
		t.Fatalf("a one-line summary got the multi-line treatment:\n%s", block)
	}
}
