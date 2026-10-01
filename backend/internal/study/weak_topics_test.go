package study

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TopicStats is the GROUP BY the repository runs, done in Go over the fake's
// tables: the caller's attempts, their answers, each answer's question's
// topic, grouped case- and space-insensitively, untagged questions left out.
func (f *fakeStore) TopicStats(_ context.Context, userID uuid.UUID) ([]TopicStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return nil, f.err
	}
	type acc struct {
		stat      TopicStat
		spellings map[string]int
		quizzes   map[uuid.UUID]struct{}
		plans     map[string]struct{}
		docs      map[string]struct{}
	}
	groups := map[string]*acc{}
	for attemptID, attempt := range f.quiz.attempts[userID] {
		quiz, ok := f.quiz.quizzes[userID][attempt.QuizID]
		if !ok {
			continue
		}
		for _, answer := range f.quiz.answers[attemptID] {
			var topic *string
			for _, q := range f.quiz.questions[attempt.QuizID] {
				if q.ID == answer.QuestionID {
					topic = q.Topic
				}
			}
			if topic == nil || strings.TrimSpace(*topic) == "" {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(*topic))
			g := groups[key]
			if g == nil {
				g = &acc{spellings: map[string]int{}, quizzes: map[uuid.UUID]struct{}{},
					plans: map[string]struct{}{}, docs: map[string]struct{}{}}
				groups[key] = g
			}
			g.spellings[strings.TrimSpace(*topic)]++
			g.stat.Answers++
			if answer.Correct {
				g.stat.Correct++
			}
			g.quizzes[quiz.ID] = struct{}{}
			if quiz.StudyPlanID != nil {
				if p, ok := f.plans[userID][*quiz.StudyPlanID]; ok {
					g.plans[p.Title] = struct{}{}
					g.stat.InActivePlan = g.stat.InActivePlan || p.Status == StatusActive
				}
			}
			if quiz.DocumentID != nil {
				if name, ok := f.documents[userID][*quiz.DocumentID]; ok {
					g.docs[name] = struct{}{}
				}
			}
			if answer.CreatedAt.After(g.stat.LastAnswered) {
				g.stat.LastAnswered = answer.CreatedAt
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []TopicStat{}
	for _, k := range keys {
		g := groups[k]
		best := 0
		for spelling, n := range g.spellings {
			if n > best || (n == best && spelling < g.stat.Topic) {
				g.stat.Topic, best = spelling, n
			}
		}
		g.stat.Quizzes = len(g.quizzes)
		g.stat.StudyPlans, g.stat.Documents = sortedKeys(g.plans), sortedKeys(g.docs)
		out = append(out, g.stat)
	}
	return out, nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the rule ---------------------------------------------------------------------

func TestTheWeakTopicRule(t *testing.T) {
	cases := []struct {
		answers, correct int
		weak             bool
		why              string
	}{
		{0, 0, false, "no evidence at all"},
		{1, 0, false, "one wrong answer is not a pattern"},
		{2, 0, false, "two answers is under the minimum, however they went"},
		{2, 1, false, "one slip in two would be 50% -- the case the minimum exists for"},
		{3, 1, true, "two wrong of three"},
		{3, 2, false, "one wrong of three is 67%"},
		{5, 3, false, "60% exactly is the bar, not below it"},
		{5, 2, true, "40%"},
		{10, 5, true, "50%"},
		{4, 0, true, "never right"},
	}
	for _, c := range cases {
		got := TopicStat{Topic: "x", Answers: c.answers, Correct: c.correct}.Weak()
		if got != c.weak {
			t.Errorf("%d of %d correct: weak = %v, want %v (%s)", c.correct, c.answers, got, c.weak, c.why)
		}
	}
}

// The property the minimum is chosen for, checked exhaustively rather than by
// example: no topic is ever weak on fewer than two wrong answers.
func TestNoTopicIsWeakOnASingleWrongAnswer(t *testing.T) {
	for n := 0; n <= 50; n++ {
		for c := 0; c <= n; c++ {
			s := TopicStat{Answers: n, Correct: c}
			if s.Weak() && s.Wrong() < 2 {
				t.Fatalf("%d of %d correct is weak on %d wrong answer(s)", c, n, s.Wrong())
			}
		}
	}
}

func TestWeakTopicsAreFilteredAndSortedWorstFirst(t *testing.T) {
	got := WeakTopicsOf([]TopicStat{
		{Topic: "strong", Answers: 10, Correct: 9},
		{Topic: "b-half", Answers: 4, Correct: 2},
		{Topic: "a-half", Answers: 4, Correct: 2},
		{Topic: "worst", Answers: 3, Correct: 0},
		{Topic: "half-more-evidence", Answers: 8, Correct: 4},
		{Topic: "too-little", Answers: 2, Correct: 0},
	})
	var names []string
	for _, s := range got {
		names = append(names, s.Topic)
	}
	want := "worst,half-more-evidence,a-half,b-half"
	if strings.Join(names, ",") != want {
		t.Fatalf("weak topics = %v, want %s", names, want)
	}
}

func TestCorrectPercentIsRounded(t *testing.T) {
	if p := (TopicStat{Answers: 3, Correct: 1}).CorrectPercent(); p != 33 {
		t.Fatalf("1 of 3 = %d%%, want 33", p)
	}
	if p := (TopicStat{Answers: 3, Correct: 2}).CorrectPercent(); p != 67 {
		t.Fatalf("2 of 3 = %d%%, want 67", p)
	}
}

// --- the service, over real attempts ------------------------------------------

// sit takes the seeded quiz once through the same calls the attempt endpoints
// make, answering each question right or wrong as told, by topic.
func sit(t *testing.T, h *harness, owner uuid.UUID, quiz Quiz, right map[string]bool) {
	t.Helper()
	ctx := context.Background()
	attempt, err := h.svc.StartAttempt(ctx, owner, quiz.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range quiz.Questions {
		pick := q.CorrectIndex
		if !right[*q.Topic] {
			pick = (q.CorrectIndex + 1) % len(q.Options)
		}
		if _, err := h.svc.SubmitAnswer(ctx, owner, attempt.ID, AnswerInput{
			QuestionID: q.ID, SelectedIndex: pick,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.CompleteAttempt(ctx, owner, attempt.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWeakTopicsComeFromTheAnswersGiven(t *testing.T) {
	h := quizHarness(t)
	quiz := seedQuiz(t, h)
	ctx := context.Background()

	// Aurora right every time; the generator wrong twice and right once.
	sit(t, h, h.owner, quiz, map[string]bool{"aurora duration": true})
	sit(t, h, h.owner, quiz, map[string]bool{"aurora duration": true})
	sit(t, h, h.owner, quiz, map[string]bool{"aurora duration": true, "generator servicing": true})

	weak, err := h.svc.WeakTopics(ctx, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(weak) != 1 {
		t.Fatalf("weak topics = %+v, want only the generator", weak)
	}
	w := weak[0]
	switch {
	case w.Topic != "generator servicing":
		t.Fatalf("topic = %q", w.Topic)
	case w.Answers != 3 || w.Correct != 1 || w.Wrong() != 2:
		t.Fatalf("counts = %d answers, %d correct", w.Answers, w.Correct)
	case w.CorrectPercent() != 33:
		t.Fatalf("percent = %d", w.CorrectPercent())
	case w.Quizzes != 1 || len(w.Documents) != 1 || w.Documents[0] != "field-notes.txt":
		t.Fatalf("provenance = %d quizzes, documents %v", w.Quizzes, w.Documents)
	case w.LastAnswered.IsZero():
		t.Fatal("no last-answered time")
	}
}

func TestOneWrongAnswerFlagsNothing(t *testing.T) {
	h := quizHarness(t)
	quiz := seedQuiz(t, h)

	sit(t, h, h.owner, quiz, map[string]bool{})
	weak, err := h.svc.WeakTopics(context.Background(), h.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(weak) != 0 {
		t.Fatalf("one attempt with every answer wrong flagged %+v", weak)
	}
}

func TestAnUnansweredQuestionIsNotAWrongOne(t *testing.T) {
	h := quizHarness(t)
	quiz := seedQuiz(t, h)
	ctx := context.Background()

	// Three attempts, each finished without touching either question.
	for range 3 {
		a, err := h.svc.StartAttempt(ctx, h.owner, quiz.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.CompleteAttempt(ctx, h.owner, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	weak, err := h.svc.WeakTopics(ctx, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(weak) != 0 {
		t.Fatalf("unanswered questions became weak topics: %+v", weak)
	}
}

func TestWeakTopicsAreTheCallersOwn(t *testing.T) {
	h := quizHarness(t)
	ctx := context.Background()
	quiz := seedQuiz(t, h)

	// A second user with their own copy of the document and quiz, who gets
	// everything wrong three times.
	other := uuid.New()
	docID := h.store.seedDocument(other, "field-notes.txt")
	h.library.seed(other, docID, "field-notes.txt", auroraPassage, generatorPassage)
	proposal, err := h.svc.ProposeQuiz(ctx, other, GenerateQuizInput{DocumentID: docID})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := h.svc.CreateQuiz(ctx, other, CreateQuizInput{
		Title: proposal.Title, DocumentID: &docID, Questions: proposal.Questions,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		sit(t, h, other, theirs, map[string]bool{})
	}
	// The owner gets everything right.
	for range 3 {
		sit(t, h, h.owner, quiz, map[string]bool{"aurora duration": true, "generator servicing": true})
	}

	mine, err := h.svc.WeakTopics(ctx, h.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 0 {
		t.Fatalf("the owner sees another user's weak topics: %+v", mine)
	}
	got, err := h.svc.WeakTopics(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the other user has %d weak topics, want 2", len(got))
	}
	for _, w := range got {
		if w.Answers != 3 || w.Correct != 0 {
			t.Fatalf("the other user's counts include the owner's answers: %+v", w)
		}
	}
}
