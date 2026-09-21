package study

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// quizHarness is newHarness with the model scripted to write a quiz, plus the
// one helper every test below needs: a quiz that exists, with its questions.
func quizHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, groundedQuizReply)
}

// seedQuiz proposes and stores a quiz, which is the path the approval flow
// takes: propose, then create from exactly what was proposed.
func seedQuiz(t *testing.T, h *harness) Quiz {
	t.Helper()
	ctx := context.Background()
	proposal, err := h.svc.ProposeQuiz(ctx, h.owner, GenerateQuizInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	docID := h.docID
	quiz, err := h.svc.CreateQuiz(ctx, h.owner, CreateQuizInput{
		Title: proposal.Title, DocumentID: &docID, Questions: proposal.Questions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiz.Questions) != 2 {
		t.Fatalf("the seeded quiz has %d questions, want 2", len(quiz.Questions))
	}
	return quiz
}

// --- generation ------------------------------------------------------------------

// The property the whole phase rests on: proposing writes nothing at all.
func TestProposingAQuizWritesNothing(t *testing.T) {
	h := quizHarness(t)

	proposal, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{
		DocumentID: h.docID, Count: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Questions) != 2 {
		t.Fatalf("proposed %d questions", len(proposal.Questions))
	}
	if proposal.DocumentID != h.docID || proposal.Filename != "field-notes.txt" {
		t.Fatalf("the proposal does not name its source: %+v", proposal)
	}
	// The source is part of the claim: which passages, not only which file.
	if len(proposal.ChunkIndexes) != 2 {
		t.Fatalf("chunk indexes = %v, want the two passages it was shown", proposal.ChunkIndexes)
	}
	if len(h.store.quizzes) != 0 {
		t.Fatalf("proposing wrote %d quizzes", len(h.store.quizzes))
	}
	found, err := h.svc.Quizzes(context.Background(), h.owner, QuizFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("the user has %d quizzes after a proposal", len(found))
	}
}

// The grounding check, from the service's side: a question whose correct
// answer is not in the document is dropped, and the drop is reported.
func TestAQuestionWithAnInventedAnswerIsDropped(t *testing.T) {
	h := newHarness(t, `[{"question":"How long did the aurora last?",`+
		`"options":["About forty minutes","Two hours"],"correct_index":0},`+
		`{"question":"What is the station's call sign?",`+
		`"options":["VP8ROT","Nothing"],"correct_index":0}]`)

	proposal, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Questions) != 1 {
		t.Fatalf("kept %d questions, want only the one the document answers: %+v",
			len(proposal.Questions), proposal.Questions)
	}
	if proposal.Dropped != 1 {
		t.Fatalf("dropped = %d, want the count reported rather than swallowed", proposal.Dropped)
	}
	if !strings.Contains(proposal.Questions[0].Question, "aurora") {
		t.Fatalf("the wrong question survived: %+v", proposal.Questions[0])
	}
}

// A distractor is *supposed* to be wrong, so it is not checked. This is the
// test that says so: every wrong option here is absent from the document, and
// the question is kept.
func TestADistractorNeedNotBeInTheDocument(t *testing.T) {
	h := newHarness(t, `[{"question":"How long did the aurora borealis last?",`+
		`"options":["About forty minutes","Six weeks of continuous daylight","A single millisecond"],`+
		`"correct_index":0}]`)

	proposal, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Questions) != 1 || proposal.Dropped != 0 {
		t.Fatalf("kept %d, dropped %d: %+v", len(proposal.Questions), proposal.Dropped, proposal.Questions)
	}
	for i, o := range proposal.Questions[0].Options {
		if i == proposal.Questions[0].CorrectIndex {
			continue
		}
		if GroundedIn(o, []string{auroraPassage, generatorPassage}) {
			t.Fatalf("this test is vacuous: the distractor %q is in the document", o)
		}
	}
}

// A whole batch the document does not answer is a failure, not an empty quiz:
// an empty quiz cannot be sat, and "here is your quiz" with nothing in it is
// worse than being told the document does not support one.
func TestAWhollyInventedQuizIsRefused(t *testing.T) {
	h := newHarness(t, `[{"question":"What is the station's call sign?",`+
		`"options":["VP8ROT","ZS6BKW"],"correct_index":0}]`)

	_, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{DocumentID: h.docID})
	if !errors.Is(err, ErrGeneration) {
		t.Fatalf("err = %v, want ErrGeneration", err)
	}
}

// The model is shown the document's own text, and the quiz prompt says where
// the answers must come from.
func TestTheQuizPromptCarriesTheDocument(t *testing.T) {
	h := quizHarness(t)
	if _, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{DocumentID: h.docID}); err != nil {
		t.Fatal(err)
	}
	body := lastUserMessage(t, h.model)
	for _, want := range []string{"aurora borealis", "fuel filter", "field-notes.txt"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the prompt does not carry %q:\n%s", want, body)
		}
	}
	prompt := ai.PromptText(h.model.LastPrompt())
	for _, want := range []string{
		"correct answer must be stated in the passages",
		"Exactly one is correct",
		// The instruction with no counterpart in the flashcard prompt.
		"none of the above",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the quiz prompt does not say %q", want)
		}
	}
}

// The same rule the flashcard example is held to, for the quiz example: if it
// shared vocabulary with the document, a model that copied it verbatim would
// sail through the grounding check and the check would measure nothing.
func TestTheQuizExampleIsAboutSomethingElse(t *testing.T) {
	example := SignificantWords(examplePassages + " " + exampleQuizReply)
	for _, w := range SignificantWords(auroraPassage + " " + generatorPassage) {
		for _, e := range example {
			if sameWord(e, w) {
				t.Fatalf("the quiz example shares %q with the test document, "+
					"so a copied question would pass the grounding check", w)
			}
		}
	}
}

// A topic narrows to the passages that match it, exactly as it does for cards,
// and one that matches nothing is refused rather than silently widened.
func TestAQuizTopicNarrowsAndCanMatchNothing(t *testing.T) {
	h := quizHarness(t)

	if _, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{
		DocumentID: h.docID, Topic: "generator",
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.library.queries); n != 1 {
		t.Fatalf("a topic made %d searches, want 1", n)
	}
	if got := h.library.queries[0].Query; got != "generator" {
		t.Fatalf("searched for %q", got)
	}

	_, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{
		DocumentID: h.docID, Topic: "photosynthesis",
	})
	if !errors.Is(err, ErrNoPassages) {
		t.Fatalf("err = %v, want ErrNoPassages", err)
	}
}

// A document that is not the caller's is a 404, and the model is never called.
func TestGeneratingAQuizFromAForeignDocumentIsNotFound(t *testing.T) {
	h := quizHarness(t)
	stranger := uuid.New()
	theirs := uuid.New()
	h.library.seed(stranger, theirs, "their-notes.txt", "Something private.")

	_, err := h.svc.ProposeQuiz(context.Background(), h.owner, GenerateQuizInput{DocumentID: theirs})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if len(h.model.Calls()) != 0 {
		t.Fatal("the model was called for somebody else's document")
	}
}

// The title is the document's when the user named none, and the user's when
// they did.
func TestAQuizIsTitledAfterItsDocumentUnlessTheUserSaysOtherwise(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()

	proposal, err := h.svc.ProposeQuiz(ctx, h.owner, GenerateQuizInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Title != "field-notes.txt" {
		t.Fatalf("title = %q, want the document's name", proposal.Title)
	}

	topical, err := h.svc.ProposeQuiz(ctx, h.owner, GenerateQuizInput{DocumentID: h.docID, Topic: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(topical.Title, "generator") {
		t.Fatalf("title = %q, want the topic in it", topical.Title)
	}

	named, err := h.svc.ProposeQuiz(ctx, h.owner, GenerateQuizInput{
		DocumentID: h.docID, Title: "  March field test  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if named.Title != "March field test" {
		t.Fatalf("title = %q, want the user's own, trimmed", named.Title)
	}
}

// --- writing -----------------------------------------------------------------------

// The approval path writes exactly what was proposed: same questions, same
// options, same order, same answer key.
func TestCreateQuizWritesExactlyWhatWasProposed(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()

	proposal, err := h.svc.ProposeQuiz(ctx, h.owner, GenerateQuizInput{DocumentID: h.docID})
	if err != nil {
		t.Fatal(err)
	}
	docID := h.docID
	quiz, err := h.svc.CreateQuiz(ctx, h.owner, CreateQuizInput{
		Title: proposal.Title, DocumentID: &docID, Questions: proposal.Questions,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiz.Questions) != len(proposal.Questions) {
		t.Fatalf("stored %d questions, proposed %d", len(quiz.Questions), len(proposal.Questions))
	}
	for i, got := range quiz.Questions {
		want := proposal.Questions[i]
		switch {
		case got.Question != want.Question:
			t.Fatalf("question %d is %q, approved %q", i, got.Question, want.Question)
		case got.CorrectIndex != want.CorrectIndex:
			t.Fatalf("question %d marks %d correct, approved %d", i, got.CorrectIndex, want.CorrectIndex)
		case strings.Join(got.Options, "|") != strings.Join(want.Options, "|"):
			// The order matters as much as the contents: an approval that
			// rearranged the options would be a different answer key.
			t.Fatalf("question %d has options %v, approved %v", i, got.Options, want.Options)
		}
	}
	// And it was stored once, not generated again.
	if len(h.model.Calls()) != 1 {
		t.Fatalf("the model was called %d times: approving regenerated", len(h.model.Calls()))
	}
}

func TestCreateQuizRefusesAForeignPlan(t *testing.T) {
	h := quizHarness(t)
	stranger := uuid.New()
	h.store.plans[stranger] = map[uuid.UUID]Plan{}
	theirs := uuid.New()
	h.store.plans[stranger][theirs] = Plan{ID: theirs, UserID: stranger, Title: "Theirs"}

	_, err := h.svc.CreateQuiz(context.Background(), h.owner, CreateQuizInput{
		Title: "Borrowed", StudyPlanID: &theirs,
		Questions: []NewQuestion{{Question: "q?", Options: []string{"a", "b"}, CorrectIndex: 0}},
	})
	if !errors.Is(err, ErrQuizNotFound) {
		t.Fatalf("err = %v, want ErrQuizNotFound", err)
	}
	if len(h.store.quizzes) != 0 {
		t.Fatal("a quiz was written for a foreign plan")
	}
}

func TestAQuizNeedsQuestions(t *testing.T) {
	h := quizHarness(t)
	_, err := h.svc.CreateQuiz(context.Background(), h.owner, CreateQuizInput{Title: "Empty"})
	var verrs validate.Errors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want a field error", err)
	}
	if verrs[0].Field != "questions" {
		t.Fatalf("field = %q", verrs[0].Field)
	}
}

// --- taking one ---------------------------------------------------------------------

func TestAnAttemptIsGradedAndScored(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)

	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case attempt.Complete():
		t.Fatal("a new attempt is already complete")
	case attempt.Score != nil:
		t.Fatalf("a new attempt has a score of %v", *attempt.Score)
	case attempt.QuestionCount != 2:
		t.Fatalf("question_count = %d, want the quiz's", attempt.QuestionCount)
	}

	// One right, one wrong.
	right, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
		QuestionID: quiz.Questions[0].ID, SelectedIndex: quiz.Questions[0].CorrectIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !right.Correct {
		t.Fatalf("the correct option was marked wrong: %+v", right)
	}
	wrong, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
		QuestionID: quiz.Questions[1].ID, SelectedIndex: otherIndex(quiz.Questions[1]),
	})
	if err != nil {
		t.Fatal(err)
	}
	if wrong.Correct {
		t.Fatalf("a wrong option was marked correct: %+v", wrong)
	}
	// The verdict comes back with the key, so the user learns the answer as
	// soon as they have committed to one.
	if wrong.CorrectIndex != quiz.Questions[1].CorrectIndex {
		t.Fatalf("correct_index = %d, want the question's", wrong.CorrectIndex)
	}

	done, err := h.svc.CompleteAttempt(ctx, h.owner, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !done.Complete():
		t.Fatal("the completed attempt is not complete")
	case done.Score == nil || *done.Score != 1:
		t.Fatalf("score = %v, want 1 of 2", done.Score)
	case len(done.Answers) != 2:
		t.Fatalf("the attempt has %d answers", len(done.Answers))
	}

	// And the quiz now reports the attempt and the best score, both computed
	// on read rather than stored.
	reloaded, err := h.svc.Quiz(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AttemptCount != 1 || reloaded.BestScore == nil || *reloaded.BestScore != 1 {
		t.Fatalf("attempts = %d, best = %v", reloaded.AttemptCount, reloaded.BestScore)
	}
}

// One answer per question per attempt. The second is a conflict rather than an
// overwrite: an attempt records what the user answered, not what they answered
// last having seen the first verdict.
func TestAQuestionCannotBeAnsweredTwiceInOneAttempt(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := AnswerInput{QuestionID: quiz.Questions[0].ID, SelectedIndex: otherIndex(quiz.Questions[0])}
	if _, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, first); err != nil {
		t.Fatal(err)
	}

	// Even with the right answer this time.
	_, err = h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
		QuestionID: quiz.Questions[0].ID, SelectedIndex: quiz.Questions[0].CorrectIndex,
	})
	if !errors.Is(err, ErrAlreadyAnswered) {
		t.Fatalf("err = %v, want ErrAlreadyAnswered", err)
	}
	done, err := h.svc.CompleteAttempt(ctx, h.owner, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *done.Score != 0 {
		t.Fatalf("score = %d: the second answer was counted", *done.Score)
	}

	// Retaking it is a new attempt, and it is a separate row.
	second, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == attempt.ID {
		t.Fatal("the second attempt reused the first")
	}
	if _, err := h.svc.SubmitAnswer(ctx, h.owner, second.ID, AnswerInput{
		QuestionID: quiz.Questions[0].ID, SelectedIndex: quiz.Questions[0].CorrectIndex,
	}); err != nil {
		t.Fatalf("the same question in a new attempt: %v", err)
	}
}

func TestAFinishedAttemptTakesNoMoreAnswersAndCompletesOnce(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.CompleteAttempt(ctx, h.owner, attempt.ID); err != nil {
		t.Fatal(err)
	}

	_, err = h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
		QuestionID: quiz.Questions[0].ID, SelectedIndex: quiz.Questions[0].CorrectIndex,
	})
	if !errors.Is(err, ErrAttemptComplete) {
		t.Fatalf("answering a finished attempt = %v, want ErrAttemptComplete", err)
	}
	if _, err := h.svc.CompleteAttempt(ctx, h.owner, attempt.ID); !errors.Is(err, ErrAttemptComplete) {
		t.Fatalf("completing twice = %v, want ErrAttemptComplete", err)
	}
}

// An attempt can be finished with questions left unanswered. They are not
// counted as wrong, they are not counted at all -- and question_count is what
// makes the score legible.
func TestAnUnfinishedAttemptCanStillBeCompleted(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
		QuestionID: quiz.Questions[0].ID, SelectedIndex: quiz.Questions[0].CorrectIndex,
	}); err != nil {
		t.Fatal(err)
	}
	done, err := h.svc.CompleteAttempt(ctx, h.owner, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *done.Score != 1 || done.QuestionCount != 2 || len(done.Answers) != 1 {
		t.Fatalf("score %v of %d from %d answers", done.Score, done.QuestionCount, len(done.Answers))
	}
}

// A question from another quiz -- including one of the caller's own -- is not
// a question in this attempt, and gets the same 404.
func TestAnAnswerMustNameAQuestionOfThisAttemptsQuiz(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	first := seedQuiz(t, h)
	second := seedQuiz(t, h)

	attempt, err := h.svc.StartAttempt(ctx, h.owner, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		id   uuid.UUID
	}{
		{"another quiz of the caller's own", second.Questions[0].ID},
		{"a question that does not exist", uuid.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{QuestionID: tc.id})
			if !errors.Is(err, ErrQuestionNotFound) {
				t.Fatalf("err = %v, want ErrQuestionNotFound", err)
			}
		})
	}
}

// An option that is not one of this question's is a field error rather than a
// 404: the question is real and the caller's, and "there is no option 7" is a
// useful thing to be told.
func TestAnOutOfRangeOptionIsAFieldError(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, index := range []int{len(quiz.Questions[0].Options), -1} {
		_, err := h.svc.SubmitAnswer(ctx, h.owner, attempt.ID, AnswerInput{
			QuestionID: quiz.Questions[0].ID, SelectedIndex: index,
		})
		var verrs validate.Errors
		if !errors.As(err, &verrs) || verrs[0].Field != "selected_index" {
			t.Fatalf("selecting option %d = %v, want a selected_index field error", index, err)
		}
	}
}

// --- isolation ------------------------------------------------------------------------

// Nothing on the quiz path ever reaches another user's rows, and every store
// call is made as the caller.
func TestQuizzesAreScopedToTheCaller(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	stranger := uuid.New()

	if found, err := h.svc.Quizzes(ctx, stranger, QuizFilter{}); err != nil || len(found) != 0 {
		t.Fatalf("a stranger sees %d quizzes (%v)", len(found), err)
	}
	for _, tc := range []struct {
		name string
		call func() error
		want error
	}{
		{"read it", func() error { _, err := h.svc.Quiz(ctx, stranger, quiz.ID); return err }, ErrQuizNotFound},
		{"delete it", func() error { return h.svc.DeleteQuiz(ctx, stranger, quiz.ID) }, ErrQuizNotFound},
		{"attempt it", func() error { _, err := h.svc.StartAttempt(ctx, stranger, quiz.ID); return err }, ErrQuizNotFound},
		{"read the attempt", func() error { _, err := h.svc.Attempt(ctx, stranger, attempt.ID); return err }, ErrAttemptNotFound},
		{"answer in it", func() error {
			_, err := h.svc.SubmitAnswer(ctx, stranger, attempt.ID, AnswerInput{
				QuestionID: quiz.Questions[0].ID, SelectedIndex: 0,
			})
			return err
		}, ErrAttemptNotFound},
		{"complete it", func() error { _, err := h.svc.CompleteAttempt(ctx, stranger, attempt.ID); return err }, ErrAttemptNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// The owner's quiz survived all of it, untouched.
	reloaded, err := h.svc.Quiz(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Questions) != 2 || reloaded.AttemptCount != 1 {
		t.Fatalf("after the stranger's attempts: %d questions, %d attempts",
			len(reloaded.Questions), reloaded.AttemptCount)
	}
	if answered, err := h.svc.Attempt(ctx, h.owner, attempt.ID); err != nil || len(answered.Answers) != 0 {
		t.Fatalf("the stranger's answer landed: %+v (%v)", answered.Answers, err)
	}
}

// Deleting a quiz takes its questions, its attempts and their answers.
func TestDeletingAQuizTakesItsAttempts(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)
	attempt, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.DeleteQuiz(ctx, h.owner, quiz.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Attempt(ctx, h.owner, attempt.ID); !errors.Is(err, ErrAttemptNotFound) {
		t.Fatalf("the attempt outlived its quiz: %v", err)
	}
	if err := h.svc.DeleteQuiz(ctx, h.owner, quiz.ID); !errors.Is(err, ErrQuizNotFound) {
		t.Fatalf("deleting it twice = %v", err)
	}
}

// otherIndex is any option of a question that is not the correct one.
func otherIndex(q Question) int {
	for i := range q.Options {
		if i != q.CorrectIndex {
			return i
		}
	}
	return q.CorrectIndex
}
