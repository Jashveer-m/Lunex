package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/study"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// What the orchestrator does with Phase 10c: weak topics as a heuristic
// source, surfaced when the message is about them and never otherwise, with
// the study service's figures and no others.

// fakeWeak is the study service's weak-topic read, keyed by owner.
type fakeWeak struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]study.TopicStat
	callers []uuid.UUID
	err     error
}

func (f *fakeWeak) WeakTopics(_ context.Context, userID uuid.UUID) ([]study.TopicStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.err != nil {
		return nil, f.err
	}
	return append([]study.TopicStat(nil), f.byUser[userID]...), nil
}

// weakHarness is newHarness with the weak-topic source wired.
func weakHarness(t *testing.T, provider *ai.Mock) (*harness, *fakeWeak) {
	t.Helper()
	h := newHarness(t, provider)
	weak := &fakeWeak{byUser: map[uuid.UUID][]study.TopicStat{}}
	h.svc = NewService(Deps{
		Store: h.store, Provider: h.provider,
		Documents: h.docs, Tasks: h.tasks, Goals: h.goals, Notes: h.notes, Calendar: h.calendar,
		WeakTopics: weak,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.svc.now = func() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }
	return h, weak
}

var lastWeek = time.Date(2026, 9, 2, 18, 30, 0, 0, time.UTC)

func mastFeed() study.TopicStat {
	return study.TopicStat{
		Topic: "mast feed timing", Answers: 5, Correct: 1, Quizzes: 2,
		StudyPlans: []string{"Kestrel relay handbook"}, Documents: []string{"relay-handbook.txt"},
		InActivePlan: true, LastAnswered: lastWeek,
	}
}

func weakSources(sources []Source) []Source {
	var out []Source
	for _, s := range sources {
		if s.Type == SourceWeakTopic {
			out = append(out, s)
		}
	}
	return out
}

// --- surfacing --------------------------------------------------------------------

// The property the phase's chat half exists for: a question about a weak topic
// puts the real figures in front of the model, and the answer can cite them.
func TestARelatedQuestionSurfacesTheWeakTopicWithItsNumbers(t *testing.T) {
	h, weak := weakHarness(t, &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.Contains(ai.PromptText(msgs), "1 of 5 answers correct (20%)") {
			return "Your quizzes show mast feed timing at 1 of 5 answers correct (20%) [S1]."
		}
		return "I found no quiz results showing a weak topic."
	}})
	weak.byUser[h.user] = []study.TopicStat{mastFeed()}

	turn, _, err := h.send(t, "Can you explain mast feed timing to me again?")
	if err != nil {
		t.Fatal(err)
	}
	got := weakSources(turn.Assistant.Sources)
	if len(got) != 1 {
		t.Fatalf("weak-topic sources = %+v, want the one topic", got)
	}
	s := got[0]
	switch {
	case s.Title != "mast feed timing":
		t.Fatalf("title = %q", s.Title)
	case s.ID != uuid.Nil:
		t.Fatalf("a computed topic carries an id: %v", s.ID)
	case !s.Cited:
		t.Fatalf("the answer cited %s and the record says otherwise", s.Label)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		`weak_topic: "mast feed timing"`,
		"1 of 5 answers correct (20%), 4 of 5 wrong (80%)",
		"across 2 quizzes",
		`under study plan "Kestrel relay handbook"`,
		"from relay-handbook.txt",
		"last answered Wed 2 Sep 2026",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(turn.Assistant.Content, "1 of 5 answers correct (20%)") {
		t.Fatalf("answer = %q", turn.Assistant.Content)
	}
}

// Naming the plan a weak topic is filed under is naming the topic's subject.
func TestNamingThePlanSurfacesItsWeakTopic(t *testing.T) {
	h, weak := weakHarness(t, nil)
	weak.byUser[h.user] = []study.TopicStat{mastFeed()}

	turn, _, err := h.send(t, "Anything I should know before I go back to the Kestrel relay handbook?")
	if err != nil {
		t.Fatal(err)
	}
	if got := weakSources(turn.Assistant.Sources); len(got) != 1 || got[0].Title != "mast feed timing" {
		t.Fatalf("weak-topic sources = %+v", got)
	}
}

// "How am I doing" names no topic; what the user is currently studying is the
// best guess at what they mean, and the list is capped.
func TestHowAmIDoingSurfacesActivePlanTopicsFirst(t *testing.T) {
	h, weak := weakHarness(t, nil)
	stale := func(topic string, correct int) study.TopicStat {
		return study.TopicStat{Topic: topic, Answers: 4, Correct: correct, Quizzes: 1, LastAnswered: lastWeek}
	}
	// Worst first, as the service returns them.
	weak.byUser[h.user] = []study.TopicStat{
		stale("eigenvalues", 0), stale("matrix rank", 0), mastFeed(), stale("determinants", 1),
	}

	turn, _, err := h.send(t, "How am I doing?")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, s := range weakSources(turn.Assistant.Sources) {
		titles = append(titles, s.Title)
	}
	if want := "mast feed timing,eigenvalues,matrix rank"; strings.Join(titles, ",") != want {
		t.Fatalf("surfaced %v, want %s", titles, want)
	}
}

// One common word is not a mention: a calendar question gets no weak topic.
func TestAnUnrelatedQuestionSurfacesNoWeakTopic(t *testing.T) {
	h, weak := weakHarness(t, nil)
	weak.byUser[h.user] = []study.TopicStat{mastFeed()}

	for _, msg := range []string{
		"What's the timing of my meeting tomorrow?",
		"Good morning!",
		"How much did I spend on food?",
	} {
		turn, _, err := h.send(t, msg)
		if err != nil {
			t.Fatal(err)
		}
		if got := weakSources(turn.Assistant.Sources); len(got) != 0 {
			t.Fatalf("%q surfaced %+v", msg, got)
		}
		if strings.Contains(ai.PromptText(h.provider.LastPrompt()), "mast feed timing") {
			t.Fatalf("%q put the weak topic in the prompt", msg)
		}
	}
}

// --- grounding --------------------------------------------------------------------

// The test the brief asks for by name: the heuristic only ever surfaces what
// the service reported, with the service's numbers, and invents nothing when
// there is nothing. Across a spread of questions, every weak-topic source is
// one of the service's topics and its figures are that topic's, exactly.
func TestTheHeuristicOnlySurfacesRealData(t *testing.T) {
	h, weak := weakHarness(t, nil)
	data := []study.TopicStat{
		mastFeed(),
		{Topic: "antenna polarisation", Answers: 7, Correct: 2, Quizzes: 1,
			Documents: []string{"relay-handbook.txt"}, LastAnswered: lastWeek},
		{Topic: "eigenvalues", Answers: 3, Correct: 0, Quizzes: 1,
			StudyPlans: []string{"Linear algebra finals"}, LastAnswered: lastWeek},
	}
	weak.byUser[h.user] = data
	byTopic := map[string]study.TopicStat{}
	for _, d := range data {
		byTopic[d.Topic] = d
	}

	questions := []string{
		"What should I study next?",
		"What am I weak at?",
		"Tell me about antenna polarisation.",
		"How is linear algebra going?",
		"Quiz me on the relay handbook.",
		"Which topics do I keep getting wrong?",
	}
	surfaced := 0
	for _, q := range questions {
		turn, _, err := h.send(t, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range weakSources(turn.Assistant.Sources) {
			surfaced++
			d, ok := byTopic[s.Title]
			if !ok {
				t.Fatalf("%q surfaced %q, which the service never reported", q, s.Title)
			}
			want := fmt.Sprintf("%d of %d answers correct (%d%%), %d of %d wrong (%d%%)",
				d.Correct, d.Answers, d.CorrectPercent(), d.Wrong(), d.Answers, 100-d.CorrectPercent())
			if !strings.HasPrefix(s.Excerpt, want) {
				t.Fatalf("%q: excerpt %q does not carry the service's figures %q", q, s.Excerpt, want)
			}
		}
	}
	if surfaced == 0 {
		t.Fatal("no question surfaced anything; the test proves nothing")
	}

	// And with nothing to report, nothing is surfaced -- not an empty
	// "weak areas" source, not a guess.
	weak.byUser[h.user] = nil
	for _, q := range questions {
		turn, _, err := h.send(t, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := weakSources(turn.Assistant.Sources); len(got) != 0 {
			t.Fatalf("%q surfaced %+v with no weak topics on record", q, got)
		}
		if strings.Contains(ai.PromptText(h.provider.LastPrompt()), "weak_topic: ") {
			t.Fatalf("%q put a weak-topic header in a prompt with none on record", q)
		}
	}
}

// The other half of the same property: the rule that forbids weak-area
// commentary with nothing behind it is in the prompt -- and in the turn that
// most invites it, a user asking what they are weak at with no data at all.
func TestTheWeakTopicRuleForbidsInventedCommentary(t *testing.T) {
	h, _ := weakHarness(t, nil)
	if _, _, err := h.send(t, "What am I weak at?"); err != nil {
		t.Fatal(err)
	}
	prompt := ai.PromptText(h.provider.LastPrompt())
	for _, want := range []string{
		fmt.Sprintf("fewer than %d%% of at least %d answers on it were right",
			int(study.WeakTopicMaxCorrectRate*100), study.WeakTopicMinAnswers),
		"repeat them exactly as the source writes them",
		"A topic is weak only if a weak_topic source names it",
		"never call any other topic weak",
		"if there is no weak_topic source in the context, do not describe their weak areas at all",
		"Do not guess why they got a topic wrong",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the weak-topic rule is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "%WEAK") {
		t.Fatal("a threshold placeholder reached the prompt")
	}
}

// --- isolation and failure ---------------------------------------------------------

func TestWeakTopicsAreReadAsTheCaller(t *testing.T) {
	h, weak := weakHarness(t, nil)
	other := uuid.New()
	weak.byUser[other] = []study.TopicStat{mastFeed()}

	turn, _, err := h.send(t, "How am I doing on mast feed timing?")
	if err != nil {
		t.Fatal(err)
	}
	if got := weakSources(turn.Assistant.Sources); len(got) != 0 {
		t.Fatalf("the caller was shown another user's weak topic: %+v", got)
	}
	for _, c := range weak.callers {
		if c != h.user {
			t.Fatalf("weak topics were read as %v, not the caller", c)
		}
	}
}

// A failed read fails the turn: "you have no weak topics" because the query
// errored is a false statement about the user's data.
func TestAWeakTopicReadFailureFailsTheTurn(t *testing.T) {
	h, weak := weakHarness(t, nil)
	weak.err = errors.New("database is down")
	if _, _, err := h.send(t, "What should I study next?"); err == nil {
		t.Fatal("the turn succeeded without its weak topics")
	}
}

// --- the tool's sources -----------------------------------------------------------

func TestAWeakTopicToolResultBecomesCitableSourcesAndSuppressesTheHeuristic(t *testing.T) {
	sources := toolSources(tools.GetWeakTopics, tools.Result{WeakTopics: []study.TopicStat{mastFeed()}})
	if len(sources) != 1 {
		t.Fatalf("sources = %+v", sources)
	}
	s := sources[0]
	if s.Type != SourceWeakTopic || s.Tool != tools.GetWeakTopics || s.Title != "mast feed timing" ||
		!strings.HasPrefix(s.Excerpt, "1 of 5 answers correct (20%), 4 of 5 wrong (80%)") {
		t.Fatalf("source = %+v", s)
	}

	h, weak := weakHarness(t, nil)
	weak.byUser[h.user] = []study.TopicStat{mastFeed()}
	got, err := h.svc.weakTopics(context.Background(), h.user, "What am I weak at?", sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("the heuristic repeated what the tool reported: %+v", got)
	}
	if len(weak.callers) != 0 {
		t.Fatal("the heuristic re-read what the tool had already read")
	}
}

// --- matching ---------------------------------------------------------------------

func TestMentions(t *testing.T) {
	cases := []struct {
		message, phrase string
		want            bool
	}{
		{"explain mast feed timing", "mast feed timing", true},
		{"what about the mast feed?", "mast feed timing", true},
		{"what's the timing of the meeting", "mast feed timing", false},
		{"eigenvalues are hard", "eigenvalues", true},
		{"tell me about eigenvalue decomposition", "eigenvalues", true},
		{"the relay handbook", "relay-handbook.txt", true},
		{"the relay", "relay-handbook.txt", false},
		{"what is a txt file", "relay-handbook.txt", false},
		{"", "mast feed timing", false},
	}
	for _, c := range cases {
		if got := mentions(matchWords(c.message), c.phrase); got != c.want {
			t.Errorf("mentions(%q, %q) = %v, want %v", c.message, c.phrase, got, c.want)
		}
	}
}

func TestAsksAboutStudy(t *testing.T) {
	for _, m := range []string{
		"How am I doing?", "How’re things -- how am I doing", "What should I study next?", "What am I weak at?",
		"Where am I struggling?", "Help me revise tonight", "I have an exam on Friday",
	} {
		if !asksAboutStudy(m) {
			t.Errorf("asksAboutStudy(%q) = false", m)
		}
	}
	for _, m := range []string{"Good morning!", "What's on tomorrow?", "How much did I spend on food?"} {
		if asksAboutStudy(m) {
			t.Errorf("asksAboutStudy(%q) = true", m)
		}
	}
}
