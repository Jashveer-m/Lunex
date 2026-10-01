package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// Phase 10c over the real router, the real service and real SQL: the weak-topic
// aggregate, whose answers it counts, and whose chat turn it reaches.
//
// These are skipped unless TEST_DATABASE_URL is set; see docs/testing.md.

// takeQuiz sits a quiz once, answering each question right or wrong by topic,
// and completes the attempt.
func takeQuiz(t *testing.T, srv *httptest.Server, token, quizID string, right map[string]bool) {
	t.Helper()
	attemptID := create(t, srv, token, "/api/v1/quizzes/"+quizID+"/attempts", nil)
	quiz := getJSON(t, srv, token, "/api/v1/quizzes/"+quizID)
	shown, _ := quiz["questions"].([]any)
	for _, raw := range shown {
		q, _ := raw.(map[string]any)
		topic := strings.ToLower(strings.TrimSpace(fmt.Sprint(q["topic"])))
		// The answer key is not on the quiz, so answer 0 and read the verdict;
		// quizBody's correct indexes are 0 for the aurora and 1 for the
		// generator, which is what picks right or wrong here.
		pick := 0
		if (topic == "generator servicing") == right[topic] {
			pick = 1
		}
		resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/answers",
			token, map[string]any{"question_id": q["id"], "selected_index": pick})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("answer: status %d: %v", resp.StatusCode, body)
		}
		if body["correct"] != right[topic] {
			t.Fatalf("answered %q %v, want correct=%v", topic, body["correct"], right[topic])
		}
	}
	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/complete", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete: status %d: %v", resp.StatusCode, body)
	}
}

func weakTopics(t *testing.T, srv *httptest.Server, token string) []map[string]any {
	t.Helper()
	body := getJSON(t, srv, token, "/api/v1/study/weak-topics")
	raw, _ := body["weak_topics"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	if count, _ := body["count"].(float64); int(count) != len(out) {
		t.Fatalf("count = %v, list has %d", body["count"], len(out))
	}
	return out
}

func TestWeakTopicsOverHTTP(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	doc := upload(t, srv, alice, "field-notes.txt", studyDocument)
	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{"title": "Field notes revision"})

	// Nothing answered yet: an empty list, and the rule beside it.
	body := getJSON(t, srv, alice, "/api/v1/study/weak-topics")
	threshold, _ := body["threshold"].(map[string]any)
	if body["count"] != float64(0) || threshold["min_answers"] != float64(3) || threshold["max_correct_rate"] != 0.6 {
		t.Fatalf("empty weak topics = %v", body)
	}

	// Two quizzes whose generator question is tagged differently in case and
	// spacing -- one topic, as far as the aggregate is concerned.
	first := quizBody("Field notes quiz")
	first["study_plan_id"], first["document_id"] = planID, doc["id"]
	quiz1 := create(t, srv, alice, "/api/v1/quizzes", first)
	second := quizBody("Field notes again")
	questions, _ := second["questions"].([]map[string]any)
	questions[1]["topic"] = "  Generator Servicing "
	quiz2 := create(t, srv, alice, "/api/v1/quizzes", second)

	// One wrong answer is not a weak topic.
	takeQuiz(t, srv, alice, quiz1, map[string]bool{"aurora duration": true})
	if got := weakTopics(t, srv, alice); len(got) != 0 {
		t.Fatalf("one wrong answer flagged %v", got)
	}

	// Three wrong of four across the two quizzes is.
	takeQuiz(t, srv, alice, quiz1, map[string]bool{"aurora duration": true})
	takeQuiz(t, srv, alice, quiz2, map[string]bool{"aurora duration": true})
	takeQuiz(t, srv, alice, quiz2, map[string]bool{"aurora duration": true, "generator servicing": true})

	got := weakTopics(t, srv, alice)
	if len(got) != 1 {
		t.Fatalf("weak topics = %v, want only the generator", got)
	}
	w := got[0]
	switch {
	case w["answers"] != float64(4) || w["correct"] != float64(1) || w["wrong"] != float64(3):
		t.Fatalf("counts = %v", w)
	case w["correct_rate"] != 0.25 || w["correct_percent"] != float64(25):
		t.Fatalf("rate = %v / %v", w["correct_rate"], w["correct_percent"])
	case w["quizzes"] != float64(2):
		t.Fatalf("quizzes = %v, want both", w["quizzes"])
	case strings.ToLower(fmt.Sprint(w["topic"])) != "generator servicing":
		t.Fatalf("topic = %v", w["topic"])
	case fmt.Sprint(w["study_plans"]) != "[Field notes revision]":
		t.Fatalf("study_plans = %v", w["study_plans"])
	case fmt.Sprint(w["documents"]) != "[field-notes.txt]":
		t.Fatalf("documents = %v", w["documents"])
	case w["last_answered_at"] == nil || w["last_answered_at"] == "":
		t.Fatalf("last_answered_at = %v", w["last_answered_at"])
	}

	resp, _ := doJSON(t, srv, http.MethodGet, "/api/v1/study/weak-topics", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", resp.StatusCode)
	}
}

func TestCrossUserWeakTopicIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	aliceQuiz := create(t, srv, alice, "/api/v1/quizzes", quizBody("Alice's quiz"))
	for range 3 {
		takeQuiz(t, srv, alice, aliceQuiz, map[string]bool{})
	}
	if got := weakTopics(t, srv, alice); len(got) != 2 {
		t.Fatalf("Alice has %d weak topics, want 2", len(got))
	}

	// Bob has answered nothing, so he has nothing -- and certainly not hers.
	if got := weakTopics(t, srv, bob); len(got) != 0 {
		t.Fatalf("Bob sees %v", got)
	}

	// Bob takes his own quiz on the same topics and gets everything right. His
	// list stays empty, and Alice's counts do not move.
	bobQuiz := create(t, srv, bob, "/api/v1/quizzes", quizBody("Bob's quiz"))
	for range 3 {
		takeQuiz(t, srv, bob, bobQuiz, map[string]bool{"aurora duration": true, "generator servicing": true})
	}
	if got := weakTopics(t, srv, bob); len(got) != 0 {
		t.Fatalf("Bob sees %v", got)
	}
	for _, w := range weakTopics(t, srv, alice) {
		if w["answers"] != float64(3) || w["correct"] != float64(0) {
			t.Fatalf("Alice's counts include Bob's answers: %v", w)
		}
	}
}

// The chat half, end to end: Alice's weak topic reaches her turn as a cited
// source with her real numbers, and Bob asking the same question gets nothing
// -- no source, and nothing about weak areas in his prompt.
func TestTheAssistantSurfacesTheCallersWeakTopicOnly(t *testing.T) {
	figures := regexp.MustCompile(`\d+ of \d+ answers correct \(\d+%\)`)
	provider := &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		prompt := ai.PromptText(msgs)
		if strings.HasPrefix(msgs[0].Content, "You are the tool selector") {
			return `{"tool": "none", "arguments": {}}`
		}
		if f := figures.FindString(prompt); f != "" && strings.Contains(prompt, "weak_topic:") {
			return "Your quizzes show generator servicing at " + f + " [S1]."
		}
		return "I found no quiz results showing a weak topic."
	}}
	srv := isolationServerWithProvider(t, provider)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	quizID := create(t, srv, alice, "/api/v1/quizzes", quizBody("Alice's quiz"))
	for range 3 {
		takeQuiz(t, srv, alice, quizID, map[string]bool{"aurora duration": true})
	}

	question := "How am I doing on generator servicing?"
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, convID, question)
	text, done := answer(t, events)
	message, _ := done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	var weak []map[string]any
	for _, raw := range sources {
		if s, _ := raw.(map[string]any); s["type"] == "weak_topic" {
			weak = append(weak, s)
		}
	}
	if len(weak) != 1 {
		t.Fatalf("weak-topic sources = %v, want the generator", sources)
	}
	if weak[0]["title"] != "generator servicing" || weak[0]["cited"] != true ||
		!strings.HasPrefix(fmt.Sprint(weak[0]["excerpt"]), "0 of 3 answers correct (0%), 3 of 3 wrong (100%)") {
		t.Fatalf("source = %v", weak[0])
	}
	if !strings.Contains(text, "0 of 3 answers correct (0%)") {
		t.Fatalf("answer = %q, want the real figures", text)
	}

	bobConv := create(t, srv, bob, "/api/v1/conversations", map[string]any{})
	_, events = ask(t, srv, bob, bobConv, question)
	text, done = answer(t, events)
	message, _ = done["message"].(map[string]any)
	sources, _ = message["sources"].([]any)
	for _, raw := range sources {
		if s, _ := raw.(map[string]any); s["type"] == "weak_topic" {
			t.Fatalf("Bob's turn surfaced Alice's weak topic: %v", s)
		}
	}
	if strings.Contains(ai.PromptText(provider.LastPrompt()), "weak_topic:") {
		t.Fatal("Bob's prompt carried a weak-topic source")
	}
	if strings.Contains(text, "answers correct") {
		t.Fatalf("Bob's answer reports figures he does not have: %q", text)
	}
}
