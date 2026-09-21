package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/study"
)

// These cover the Phase 10b SQL: the three counts that are computed rather
// than stored, the question order that only holds because of
// clock_timestamp(), the unique index that makes "answer twice" impossible
// rather than merely checked, the score computed in the statement that closes
// an attempt, and the four cascades. Cross-user isolation over the whole stack
// lives in internal/api/quiz_isolation_test.go.

// quizQuestions is the questions the tests below write. There are enough of
// them that an ordering bug shows up rather than being a coin flip, and they
// are numbered so a wrong order is readable in the failure message.
func quizQuestions(n int) []study.NewQuestion {
	out := make([]study.NewQuestion, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, study.NewQuestion{
			Question:     "Question " + string(rune('a'+i)) + "?",
			Options:      []string{"first", "second", "third"},
			CorrectIndex: i % 3,
			Topic:        "topic " + string(rune('a'+i)),
		})
	}
	return out
}

func TestQuizRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	doc := makeDocument(t, docs, owner, "field-notes.txt", studyPassages)
	plan, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{
		Title: "Field notes revision", DocumentID: &doc.ID, Status: study.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Field notes quiz", StudyPlanID: &plan.ID, DocumentID: &doc.ID,
		Questions: quizQuestions(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case created.Title != "Field notes quiz":
		t.Fatalf("title = %q", created.Title)
	// Both links come back with their titles on the join, so a client renders
	// a row without a second request.
	case created.StudyPlanTitle == nil || *created.StudyPlanTitle != "Field notes revision":
		t.Fatalf("study_plan = %v", created.StudyPlanTitle)
	case created.DocumentName == nil || *created.DocumentName != "field-notes.txt":
		t.Fatalf("document = %v", created.DocumentName)
	// The three derived values, on a quiz nobody has taken.
	case created.QuestionCount != 8:
		t.Fatalf("question_count = %d", created.QuestionCount)
	case created.AttemptCount != 0:
		t.Fatalf("attempt_count = %d", created.AttemptCount)
	case created.BestScore != nil:
		t.Fatalf("best_score = %v, want null before any attempt is finished", *created.BestScore)
	}

	// The question order, which is the Phase 10a ordering bug's second
	// appearance: the rows are inserted in one transaction, so now() would
	// give every one of them the same stamp and the read order would fall to
	// the tie-break on a random uuid. clock_timestamp() is what keeps a quiz
	// in the order it was written -- and therefore of the document it came
	// from. Eight questions make a shuffle unmissable.
	if len(created.Questions) != 8 {
		t.Fatalf("%d questions came back", len(created.Questions))
	}
	for i, q := range created.Questions {
		want := "Question " + string(rune('a'+i)) + "?"
		if q.Question != want {
			t.Fatalf("question %d is %q, want %q -- the quiz read back out of order", i, q.Question, want)
		}
		if len(q.Options) != 3 || q.Options[0] != "first" {
			t.Fatalf("options %d = %v, want the jsonb array round-tripped", i, q.Options)
		}
		if q.CorrectIndex != i%3 {
			t.Fatalf("correct_index %d = %d", i, q.CorrectIndex)
		}
		if q.Topic == nil || *q.Topic != "topic "+string(rune('a'+i)) {
			t.Fatalf("topic %d = %v -- the column 10c reads did not round-trip", i, q.Topic)
		}
	}

	// A question with no topic stores NULL rather than an empty string, so
	// "not tagged" has exactly one representation.
	untagged, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Untagged", Questions: []study.NewQuestion{
			{Question: "q?", Options: []string{"a", "b"}, CorrectIndex: 0},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if untagged.Questions[0].Topic != nil {
		t.Fatalf("topic = %v, want NULL", *untagged.Questions[0].Topic)
	}

	// The list carries the counts and no questions: a list of titles is what
	// it is for, and loading every quiz's questions would put the answers
	// somewhere nobody asked for them.
	found, err := repo.Quizzes(ctx, owner, study.QuizFilter{
		Sort: study.DefaultQuizSort, Limit: study.DefaultQuizLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("%d quizzes listed", len(found))
	}
	for _, q := range found {
		if len(q.Questions) != 0 {
			t.Fatalf("a listed quiz carries %d questions", len(q.Questions))
		}
	}

	// The filters, each narrowing to the quizzes that name the link.
	for name, f := range map[string]study.QuizFilter{
		"by plan":     {StudyPlanID: &plan.ID},
		"by document": {DocumentID: &doc.ID},
		"by title":    {Query: "field notes"},
	} {
		t.Run(name, func(t *testing.T) {
			f.Sort, f.Limit = study.DefaultQuizSort, study.DefaultQuizLimit
			got, err := repo.Quizzes(ctx, owner, f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ID != created.ID {
				t.Fatalf("matched %+v", got)
			}
		})
	}
}

// The grading path: one attempt, some answers, a score computed in the same
// statement that closes it.
func TestQuizAttemptRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	quiz, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Field notes quiz", Questions: quizQuestions(4),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repo.CreateAttempt(ctx, owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case attempt.QuizTitle != "Field notes quiz":
		t.Fatalf("quiz title = %q, want it joined on", attempt.QuizTitle)
	case attempt.QuestionCount != 4:
		t.Fatalf("question_count = %d", attempt.QuestionCount)
	case attempt.CompletedAt != nil || attempt.Score != nil:
		t.Fatalf("a new attempt is %v / %v", attempt.CompletedAt, attempt.Score)
	}

	// Two right, one wrong, one left unanswered.
	for i, correct := range []bool{true, true, false} {
		q := quiz.Questions[i]
		selected := q.CorrectIndex
		if !correct {
			selected = (q.CorrectIndex + 1) % len(q.Options)
		}
		answer, err := repo.CreateAnswer(ctx, owner, attempt.ID,
			study.AnswerInput{QuestionID: q.ID, SelectedIndex: selected}, correct)
		if err != nil {
			t.Fatal(err)
		}
		// The answer comes back joined to its question, which is what makes a
		// review of an attempt a review rather than a list of integers.
		if answer.Question != q.Question || answer.CorrectIndex != q.CorrectIndex {
			t.Fatalf("answer %d = %+v, want the question joined on", i, answer)
		}
		if len(answer.Options) != 3 {
			t.Fatalf("answer %d has %d options", i, len(answer.Options))
		}
	}

	// The unique index. The service checks first -- it has a better error --
	// but a check is a race and a constraint is not.
	if _, err := repo.CreateAnswer(ctx, owner, attempt.ID,
		study.AnswerInput{QuestionID: quiz.Questions[0].ID, SelectedIndex: 0}, true,
	); !errors.Is(err, study.ErrAlreadyAnswered) {
		t.Fatalf("answering twice = %v, want ErrAlreadyAnswered", err)
	}

	done, err := repo.CompleteAttempt(ctx, owner, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case done.Score == nil || *done.Score != 2:
		t.Fatalf("score = %v, want 2 -- the unanswered question is not counted wrong, it is not counted", done.Score)
	case done.CompletedAt == nil:
		t.Fatalf("the completed attempt has no completed_at")
	case len(done.Answers) != 3:
		t.Fatalf("%d answers", len(done.Answers))
	}
	// Oldest first, which is the order they were given in -- and holds only
	// because the column is stamped with clock_timestamp().
	for i, a := range done.Answers {
		if a.QuestionID != quiz.Questions[i].ID {
			t.Fatalf("answer %d is for %v, want the questions in the order they were answered", i, a.QuestionID)
		}
	}

	// `completed_at IS NULL` in the WHERE is what makes completion once-only,
	// and an answer to a closed attempt inserts nothing.
	if _, err := repo.CompleteAttempt(ctx, owner, attempt.ID); !errors.Is(err, study.ErrAttemptComplete) {
		t.Fatalf("completing twice = %v, want ErrAttemptComplete", err)
	}
	if _, err := repo.CreateAnswer(ctx, owner, attempt.ID,
		study.AnswerInput{QuestionID: quiz.Questions[3].ID, SelectedIndex: 0}, true,
	); !errors.Is(err, study.ErrAttemptNotFound) {
		t.Fatalf("answering a closed attempt = %v", err)
	}

	// A second attempt scoring better moves the quiz's best score, which is a
	// max over the owner's finished attempts and is computed on read.
	second, err := repo.CreateAttempt(ctx, owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range quiz.Questions {
		if _, err := repo.CreateAnswer(ctx, owner, second.ID,
			study.AnswerInput{QuestionID: q.ID, SelectedIndex: q.CorrectIndex}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.CompleteAttempt(ctx, owner, second.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repo.QuizByID(ctx, owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AttemptCount != 2 || reloaded.BestScore == nil || *reloaded.BestScore != 4 {
		t.Fatalf("attempts = %d, best = %v", reloaded.AttemptCount, reloaded.BestScore)
	}
}

// Every statement carries the owner, including the three that reach rows with
// no user_id of their own.
func TestQuizRepositoryRefusesForeignRows(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	ada := makeUser(t, pool, "ada@example.com")
	bob := makeUser(t, pool, "bob@example.com")

	quiz, err := repo.CreateQuiz(ctx, ada, study.CreateQuizInput{
		Title: "Ada's quiz", Questions: quizQuestions(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repo.CreateAttempt(ctx, ada, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		call func() error
		want error
	}{
		"read it":          {func() error { _, err := repo.QuizByID(ctx, bob, quiz.ID); return err }, study.ErrQuizNotFound},
		"delete it":        {func() error { return repo.DeleteQuiz(ctx, bob, quiz.ID) }, study.ErrQuizNotFound},
		"attempt it":       {func() error { _, err := repo.CreateAttempt(ctx, bob, quiz.ID); return err }, study.ErrQuizNotFound},
		"read the attempt": {func() error { _, err := repo.AttemptByID(ctx, bob, attempt.ID); return err }, study.ErrAttemptNotFound},
		"complete it":      {func() error { _, err := repo.CompleteAttempt(ctx, bob, attempt.ID); return err }, study.ErrAttemptNotFound},
		"read its question": {func() error {
			_, err := repo.QuestionForAttempt(ctx, bob, attempt.ID, quiz.Questions[0].ID)
			return err
		}, study.ErrQuestionNotFound},
		"answer in it": {func() error {
			_, err := repo.CreateAnswer(ctx, bob, attempt.ID,
				study.AnswerInput{QuestionID: quiz.Questions[0].ID, SelectedIndex: 0}, true)
			return err
		}, study.ErrAttemptNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}

	// A question of Ada's quiz is not a question of an attempt at Bob's, even
	// for Bob's own attempt: it is reached through the attempt, so the join
	// finds nothing.
	bobsQuiz, err := repo.CreateQuiz(ctx, bob, study.CreateQuizInput{
		Title: "Bob's quiz", Questions: quizQuestions(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	bobsAttempt, err := repo.CreateAttempt(ctx, bob, bobsQuiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.QuestionForAttempt(ctx, bob, bobsAttempt.ID, quiz.Questions[0].ID); !errors.Is(err, study.ErrQuestionNotFound) {
		t.Fatalf("reaching another user's question through an owned attempt = %v", err)
	}

	// And none of it touched Ada's rows.
	after, err := repo.QuizByID(ctx, ada, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AttemptCount != 1 || len(after.Questions) != 2 {
		t.Fatalf("Ada's quiz after Bob's attempts: %d attempts, %d questions",
			after.AttemptCount, len(after.Questions))
	}
	if reread, err := repo.AttemptByID(ctx, ada, attempt.ID); err != nil || len(reread.Answers) != 0 {
		t.Fatalf("Bob's answer landed in Ada's attempt: %+v (%v)", reread.Answers, err)
	}
}

// The four cascades, each of which is a different decision: the plan takes its
// quizzes, the document does not, the quiz takes its questions and attempts,
// and the user takes everything.
func TestQuizCascades(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	doc := makeDocument(t, docs, owner, "field-notes.txt", studyPassages)
	plan, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{
		Title: "Field notes revision", Status: study.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	// One quiz under the plan, one under nothing; both from the document.
	underPlan, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Under the plan", StudyPlanID: &plan.ID, DocumentID: &doc.ID,
		Questions: quizQuestions(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	standalone, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "On its own", DocumentID: &doc.ID, Questions: quizQuestions(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := repo.CreateAttempt(ctx, owner, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateAnswer(ctx, owner, attempt.ID,
		study.AnswerInput{QuestionID: standalone.Questions[0].ID, SelectedIndex: 0}, true); err != nil {
		t.Fatal(err)
	}

	// Deleting the document leaves both quizzes and unlinks them. The
	// studying happened; the source file is a convenience.
	if err := docs.Delete(ctx, owner, doc.ID); err != nil {
		t.Fatal(err)
	}
	orphaned, err := repo.QuizByID(ctx, owner, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orphaned.DocumentID != nil || orphaned.DocumentName != nil {
		t.Fatalf("document = %v/%v, want null once it is gone", orphaned.DocumentID, orphaned.DocumentName)
	}
	if len(orphaned.Questions) != 2 {
		t.Fatalf("the deleted document took %d questions with it", 2-len(orphaned.Questions))
	}

	// Deleting the plan takes the quiz filed under it, and leaves the one that
	// was not.
	if err := repo.DeletePlan(ctx, owner, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.QuizByID(ctx, owner, underPlan.ID); !errors.Is(err, study.ErrQuizNotFound) {
		t.Fatalf("the plan's quiz survived it: %v", err)
	}
	if _, err := repo.QuizByID(ctx, owner, standalone.ID); err != nil {
		t.Fatalf("the standalone quiz went with the plan: %v", err)
	}

	// Deleting the quiz takes its questions, its attempts and their answers.
	if err := repo.DeleteQuiz(ctx, owner, standalone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AttemptByID(ctx, owner, attempt.ID); !errors.Is(err, study.ErrAttemptNotFound) {
		t.Fatalf("the attempt outlived its quiz: %v", err)
	}
	for _, table := range []string{"quiz_questions", "quiz_answers"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s after the quiz was deleted", n, table)
		}
	}

	// And the user takes everything.
	if _, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Last one", Questions: quizQuestions(1),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"quizzes", "quiz_questions", "quiz_attempts", "quiz_answers"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s after the user was deleted", n, table)
		}
	}
}

// A quiz has no knowledge-graph node and no trigger, so nothing is written
// into the graph and nothing has to be cleaned out of it. This is the
// assertion that says migration 000011 left the allow-list alone on purpose.
func TestAQuizIsNotMirroredIntoTheGraph(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	quiz, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Field notes quiz", Questions: quizQuestions(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(
		`SELECT count(*) FROM knowledge_nodes WHERE ref_id = $1`, quiz.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d nodes point at the quiz", n)
	}
	// And the allow-list still refuses one, which is what makes "quizzes get
	// no node" a property of the schema rather than of the Go code.
	_, err = pool.Exec(`
		INSERT INTO knowledge_nodes (user_id, type, label, ref_table, ref_id)
		VALUES ($1, 'project', 'Field notes quiz', 'quizzes', $2)`, owner, quiz.ID)
	if err == nil {
		t.Fatal("the graph accepted a node whose ref_table is 'quizzes'")
	}
}
