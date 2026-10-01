package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/study"
)

// These cover the Phase 10c SQL: the one GROUP BY over quiz answers by their
// question's topic. Cross-user isolation over the whole stack, and the chat
// turn that reads it, live in internal/api/weak_topics_isolation_test.go.

// answerAll starts an attempt and answers the given questions, each right or
// wrong as told, leaving the attempt open unless complete is set.
func answerAll(t *testing.T, repo *study.Repository, owner uuid.UUID, quiz study.Quiz, right []bool, complete bool) {
	t.Helper()
	ctx := context.Background()
	attempt, err := repo.CreateAttempt(ctx, owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, correct := range right {
		q := quiz.Questions[i]
		selected := q.CorrectIndex
		if !correct {
			selected = (q.CorrectIndex + 1) % len(q.Options)
		}
		if _, err := repo.CreateAnswer(ctx, owner, attempt.ID,
			study.AnswerInput{QuestionID: q.ID, SelectedIndex: selected}, correct); err != nil {
			t.Fatal(err)
		}
	}
	if complete {
		if _, err := repo.CompleteAttempt(ctx, owner, attempt.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func question(topic string) study.NewQuestion {
	return study.NewQuestion{
		Question: "About " + topic + "?", Options: []string{"right", "wrong", "also wrong"},
		CorrectIndex: 0, Topic: topic,
	}
}

func statByTopic(stats []study.TopicStat) map[string]study.TopicStat {
	out := map[string]study.TopicStat{}
	for _, s := range stats {
		out[strings.ToLower(s.Topic)] = s
	}
	return out
}

func TestTopicStatsAggregatesAnswersByTopic(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := study.NewRepository(pool)
	docs := documents.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	stranger := makeUser(t, pool, "eve@example.com")
	doc := makeDocument(t, docs, owner, "relay-handbook.txt", studyPassages)
	plan, err := repo.CreatePlan(ctx, owner, study.CreatePlanInput{
		Title: "Kestrel relay handbook", DocumentID: &doc.ID, Status: study.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Two quizzes. The mast-feed topic is spelled two ways; one question in
	// the second has no topic at all.
	first, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title: "Relay quiz", StudyPlanID: &plan.ID, DocumentID: &doc.ID,
		Questions: []study.NewQuestion{question("mast feed timing"), question("antenna polarisation")},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.CreateQuiz(ctx, owner, study.CreateQuizInput{
		Title:     "Relay quiz, again",
		Questions: []study.NewQuestion{question("Mast Feed Timing"), question("")},
	})
	if err != nil {
		t.Fatal(err)
	}

	answerAll(t, repo, owner, first, []bool{false, true}, true)
	answerAll(t, repo, owner, first, []bool{false, true}, true)
	// An open attempt's answers are graded already, so they count.
	answerAll(t, repo, owner, second, []bool{true, false}, false)
	// An attempt that answered nothing contributes nothing.
	answerAll(t, repo, owner, first, nil, true)

	// A stranger with a quiz on the same topic, answered wrong three times.
	theirs, err := repo.CreateQuiz(ctx, stranger, study.CreateQuizInput{
		Title: "Eve's quiz", Questions: []study.NewQuestion{question("mast feed timing")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		answerAll(t, repo, stranger, theirs, []bool{false}, true)
	}

	stats, err := repo.TopicStats(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	byTopic := statByTopic(stats)
	if len(byTopic) != 2 {
		t.Fatalf("topics = %+v, want mast feed and antenna, and no untagged group", stats)
	}
	mast := byTopic["mast feed timing"]
	switch {
	case mast.Answers != 3 || mast.Correct != 1:
		t.Fatalf("mast feed = %d answers, %d correct; want the owner's 3 and 1 only", mast.Answers, mast.Correct)
	case mast.Topic != "mast feed timing":
		// Two answers were to the lower-case spelling, one to the other.
		t.Fatalf("topic spelled %q, want the most common spelling", mast.Topic)
	case mast.Quizzes != 2:
		t.Fatalf("quizzes = %d", mast.Quizzes)
	case len(mast.StudyPlans) != 1 || mast.StudyPlans[0] != "Kestrel relay handbook":
		t.Fatalf("study plans = %v", mast.StudyPlans)
	case len(mast.Documents) != 1 || mast.Documents[0] != "relay-handbook.txt":
		t.Fatalf("documents = %v", mast.Documents)
	case !mast.InActivePlan:
		t.Fatal("the topic's plan is active and the stat says otherwise")
	case mast.LastAnswered.IsZero():
		t.Fatal("no last-answered time")
	case !mast.Weak():
		t.Fatalf("1 of 3 is not weak: %+v", mast)
	}
	if ant := byTopic["antenna polarisation"]; ant.Answers != 2 || ant.Correct != 2 || ant.Weak() {
		t.Fatalf("antenna = %+v", ant)
	}

	// The stranger sees their own three answers and nothing of the owner's.
	theirStats, err := repo.TopicStats(ctx, stranger)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirStats) != 1 || theirStats[0].Answers != 3 || theirStats[0].Correct != 0 ||
		len(theirStats[0].StudyPlans) != 0 {
		t.Fatalf("the stranger's stats = %+v", theirStats)
	}

	// Finishing the plan is what "not currently studying" means.
	if _, err := pool.Exec(`UPDATE study_plans SET status = 'completed' WHERE id = $1`, plan.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting the document keeps the evidence and loses only the link -- the
	// quiz survives its source, and so do the answers to it.
	if err := docs.Delete(ctx, owner, doc.ID); err != nil {
		t.Fatal(err)
	}
	stats, err = repo.TopicStats(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	mast = statByTopic(stats)["mast feed timing"]
	switch {
	case mast.Answers != 3:
		t.Fatalf("answers = %d after deleting the document", mast.Answers)
	case mast.InActivePlan:
		t.Fatal("a completed plan still counts as active")
	case len(mast.Documents) != 0:
		t.Fatalf("documents = %v after the document was deleted", mast.Documents)
	}
}

// A user with no answers has no stats, as an empty list rather than nil.
func TestTopicStatsIsEmptyWithNoAnswers(t *testing.T) {
	pool := testDB(t)
	repo := study.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	stats, err := repo.TopicStats(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if stats == nil || len(stats) != 0 {
		t.Fatalf("stats = %#v, want an empty list", stats)
	}
}
