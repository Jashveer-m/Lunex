package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/study"
)

// --- search_quizzes ----------------------------------------------------------

func TestSearchQuizzesFindsAndRunsWithoutApproval(t *testing.T) {
	w := newWorld()
	w.study.seedQuiz(w.user, "Kestrel relay handbook", 6)
	w.study.seedQuiz(w.user, "Operating systems", 4)

	call, err := w.reg.Prepare(context.Background(), w.user, SearchQuizzes, Args{"query": "relay"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Read {
		t.Fatalf("permission = %q", call.Permission)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, call)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Quizzes) != 1 || result.Quizzes[0].Title != "Kestrel relay handbook" {
		t.Fatalf("found %+v", result.Quizzes)
	}
	if w.totalWrites() != 0 {
		t.Fatal("a search wrote something")
	}
}

// What a quiz read reports, and -- the part that matters -- what it does not.
// A quiz the assistant can quote back is a quiz it can spoil.
func TestSearchQuizzesReportsNoQuestionsAndNoAnswers(t *testing.T) {
	w := newWorld()
	call, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, Args{"document": "field-notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	id := w.ledger.add(w.user, call, "proposed")
	if _, err := w.reg.RunApproved(context.Background(), w.user, id); err != nil {
		t.Fatal(err)
	}

	read, err := w.reg.Prepare(context.Background(), w.user, SearchQuizzes, Args{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := w.reg.RunRead(context.Background(), w.user, read)
	if err != nil {
		t.Fatal(err)
	}
	output := string(mustJSON(t, result.Output))
	for _, forbidden := range []string{
		"How long did the aurora last?", "About forty minutes", "correct_index", "options",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("a quiz read leaked %q:\n%s", forbidden, output)
		}
	}
	for _, want := range []string{"question_count", "attempt_count", "best_score"} {
		if !strings.Contains(output, want) {
			t.Fatalf("a quiz read does not report %q:\n%s", want, output)
		}
	}
}

// --- generate_quiz ---------------------------------------------------------------

// The proposal is the quiz. Nothing is written, and what the user reads is the
// actual questions with their options and which one will be marked right.
func TestGenerateQuizProposesTheQuestionsThemselves(t *testing.T) {
	w := newWorld()

	call, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, Args{
		"document": "field-notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Permission != Write {
		t.Fatalf("permission = %q", call.Permission)
	}
	if w.totalWrites() != 0 {
		t.Fatalf("preparing wrote %d times", w.totalWrites())
	}
	// Generation happened once, during preparation.
	if len(w.study.quizProposals) != 1 {
		t.Fatalf("%d generations while preparing, want 1", len(w.study.quizProposals))
	}
	// The questions are in the stored input, answer key and all.
	var in struct {
		Title     string `json:"title"`
		Questions []struct {
			Question     string   `json:"question"`
			Options      []string `json:"options"`
			CorrectIndex int      `json:"correct_index"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		t.Fatal(err)
	}
	if len(in.Questions) != 2 || in.Title == "" {
		t.Fatalf("input carries %d questions and the title %q: %s", len(in.Questions), in.Title, call.Input)
	}
	// And in the summary, in full: every question, every option, and the one
	// that is going to be marked correct. An answer key the user never saw is
	// the one thing here they cannot correct afterwards.
	for _, want := range []string{
		"How long did the aurora last?", "About forty minutes", "Two hours",
		"A new fuel filter", "(correct)", "field-notes.txt",
	} {
		if !strings.Contains(call.Summary, want) {
			t.Fatalf("summary does not show %q:\n%s", want, call.Summary)
		}
	}
	// The correct option is the one marked, not just some option.
	for _, line := range strings.Split(call.Summary, "\n") {
		if strings.Contains(line, "(correct)") && strings.Contains(line, "Two hours") {
			t.Fatalf("the summary marks the wrong option: %q", line)
		}
	}
	// It closes with a sentence rather than a full stop stuck on the last
	// option, which would read as part of the option -- and the sentence says
	// what the marker means, on the card where the answer key is approved.
	if !strings.HasSuffix(call.Summary, "The option marked (correct) is the one the quiz will mark right.") {
		t.Fatalf("summary does not close with what the marker means:\n%s", call.Summary)
	}
	// The default title is the document's name, so the file is not named
	// twice in one sentence.
	if strings.Count(call.Summary, "field-notes.txt") != 1 {
		t.Fatalf("the summary names the document %d times:\n%s",
			strings.Count(call.Summary, "field-notes.txt"), call.Summary)
	}
}

// A title the user gave still names the document it was made from: where the
// questions came from is half of what is being approved.
func TestAUserTitledQuizStillNamesItsDocument(t *testing.T) {
	w := newWorld()
	call, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, Args{
		"document": "field-notes.txt", "title": "March field test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call.Summary, `"March field test" made from "field-notes.txt"`) {
		t.Fatalf("summary = %q", call.Summary)
	}
}

// Approving writes exactly the quiz that was shown, and does not generate
// again -- which would store a quiz nobody had read.
func TestApprovingAQuizWritesWhatWasShownAndDoesNotRegenerate(t *testing.T) {
	w := newWorld()
	call, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, Args{
		"document": "field-notes.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.reg.RunRead(context.Background(), w.user, call); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}

	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatalf("approving: %v / %v", err, exec.Err)
	}
	if len(w.study.quizProposals) != 1 {
		t.Fatalf("approving generated again: %d generations", len(w.study.quizProposals))
	}
	if len(w.study.created) != 1 {
		t.Fatalf("wrote %+v", w.study.created)
	}
	written := w.study.created[0]
	if written.DocumentID == nil {
		t.Fatalf("the quiz does not record its source: %+v", written)
	}
	// Decoded through the tool's own wire names -- study.NewQuestion has no
	// JSON tags, and reading `correct_index` as the zero value would make this
	// assertion pass for the wrong reason.
	var in struct {
		Questions []quizQuestionInput `json:"questions"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		t.Fatal(err)
	}
	for i, got := range written.Questions {
		want := in.Questions[i]
		if got.Question != want.Question || got.CorrectIndex != want.CorrectIndex ||
			strings.Join(got.Options, "|") != strings.Join(want.Options, "|") {
			t.Fatalf("question %d written as %+v, approved %+v", i, got, want)
		}
	}
	// The recorded result names the quiz and the questions, and carries no
	// answers: it is read back into later prompts.
	output := string(mustJSON(t, exec.Result.Output))
	if !strings.Contains(output, "How long did the aurora last?") {
		t.Fatalf("result = %s", output)
	}
	for _, forbidden := range []string{"correct_index", "About forty minutes", "Two hours"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("the recorded result leaks %q:\n%s", forbidden, output)
		}
	}
}

func TestAQuizIsFiledUnderANamedPlan(t *testing.T) {
	w := newWorld()
	plan := w.study.seedPlan(w.user, "Field notes revision")

	call, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, Args{
		"document": "field-notes.txt", "study_plan": "field notes revision",
		"count": "1", "title": "March field test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(call.Summary, "Field notes revision") ||
		!strings.Contains(call.Summary, "March field test") {
		t.Fatalf("summary = %q", call.Summary)
	}
	if n := w.study.quizProposals[0].Count; n != 1 {
		t.Fatalf("asked the service for %d questions, want 1", n)
	}
	id := w.ledger.add(w.user, call, "proposed")
	if _, err := w.reg.RunApproved(context.Background(), w.user, id); err != nil {
		t.Fatal(err)
	}
	written := w.study.created[0]
	if written.StudyPlanID == nil || *written.StudyPlanID != plan.ID {
		t.Fatalf("filed under %v, want %v", written.StudyPlanID, plan.ID)
	}
}

// Every way the preparation can be refused is an argument error the answering
// model can turn into a question, and none of them writes anything.
func TestGenerateQuizRefusalsAreArgumentErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args    Args
		seed    func(*world)
		mention string
	}{
		"no document named": {
			args: Args{}, mention: "which uploaded document",
		},
		"a document the user does not have": {
			args: Args{"document": "lecture-3.pdf"}, mention: "lecture-3.pdf",
		},
		"a plan the user does not have": {
			args:    Args{"document": "field-notes.txt", "study_plan": "thermodynamics"},
			mention: "thermodynamics",
		},
		"a document with no indexed text": {
			args:    Args{"document": "field-notes.txt"},
			seed:    func(w *world) { w.study.proposeQuizErr = study.ErrNotStudyable },
			mention: "finished processing",
		},
		"a topic the document does not cover": {
			args:    Args{"document": "field-notes.txt", "topic": "photosynthesis"},
			seed:    func(w *world) { w.study.proposeQuizErr = study.ErrNoPassages },
			mention: "photosynthesis",
		},
		"nothing the document actually answers": {
			args:    Args{"document": "field-notes.txt"},
			seed:    func(w *world) { w.study.proposeQuizErr = study.ErrGeneration },
			mention: "does not have enough",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld()
			if tc.seed != nil {
				tc.seed(w)
			}
			_, err := w.reg.Prepare(context.Background(), w.user, GenerateQuiz, tc.args)
			if !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("err = %v, want an argument error", err)
			}
			if !strings.Contains(err.Error(), tc.mention) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.mention)
			}
			if !strings.Contains(err.Error(), GenerateQuiz) {
				t.Fatalf("err = %v, want it to name the tool the user asked for", err)
			}
			if w.totalWrites() != 0 {
				t.Fatal("a refused call wrote something")
			}
		})
	}
}

// There is no tool that takes a quiz. This pins it by name rather than by
// reading the list: the guarantee is that no registered tool can start an
// attempt, answer a question or finish one.
func TestNoToolCanTakeAQuiz(t *testing.T) {
	w := newWorld()
	for _, name := range []string{
		"start_quiz_attempt", "answer_quiz_question", "submit_answer",
		"complete_quiz_attempt", "take_quiz", "score_quiz",
	} {
		if _, ok := w.reg.Tool(name); ok {
			t.Fatalf("%s is registered; taking a quiz is the user's own action", name)
		}
	}
	for _, tool := range w.reg.Tools() {
		for _, word := range []string{"attempt", "answer", "grade", "score"} {
			if strings.Contains(tool.Name, word) {
				t.Fatalf("%s is registered; taking a quiz is the user's own action", tool.Name)
			}
		}
	}
}
