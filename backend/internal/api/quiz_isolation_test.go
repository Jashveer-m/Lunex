package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Phase 10b half of the property the whole ownership design exists for,
// over the real router, the real service and real SQL -- plus the grading
// rules, which are the part of this phase that is not CRUD.
//
// These are skipped unless TEST_DATABASE_URL is set; see docs/testing.md.

// quizBody is a quiz a client writes directly, which is the same path an
// approved generate_quiz takes. Its answers are drawn from studyDocument, so
// the grounding assertion at the bottom means something.
func quizBody(title string) map[string]any {
	return map[string]any{
		"title": title,
		"questions": []map[string]any{
			{
				"question":      "How long did the aurora borealis last?",
				"options":       []string{"About forty minutes", "Two hours", "Until sunrise"},
				"correct_index": 0,
				"topic":         "aurora duration",
			},
			{
				"question":      "What does the generator need before the next resupply run?",
				"options":       []string{"A spare alternator", "A new fuel filter"},
				"correct_index": 1,
				"topic":         "generator servicing",
			},
		},
	}
}

func getJSON(t *testing.T, srv *httptest.Server, token, path string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, path, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %v", path, resp.StatusCode, body)
	}
	return body
}

// quizQuestionIDs is the ids of a quiz's questions, in order.
func quizQuestionIDs(t *testing.T, quiz map[string]any) []string {
	t.Helper()
	raw, _ := quiz["questions"].([]any)
	out := make([]string, 0, len(raw))
	for _, q := range raw {
		question, _ := q.(map[string]any)
		out = append(out, fmt.Sprint(question["id"]))
	}
	return out
}

func TestCrossUserQuizIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	quizID := create(t, srv, alice, "/api/v1/quizzes", quizBody("Alice's quiz"))
	quiz := getJSON(t, srv, alice, "/api/v1/quizzes/"+quizID)
	questions := quizQuestionIDs(t, quiz)
	attemptID := create(t, srv, alice, "/api/v1/quizzes/"+quizID+"/attempts", nil)

	// Alice's rows exist and Bob does not own them. The answer must be 404 --
	// a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get quiz", http.MethodGet, "/api/v1/quizzes/" + quizID, nil},
		{"delete quiz", http.MethodDelete, "/api/v1/quizzes/" + quizID, nil},
		{"attempt it", http.MethodPost, "/api/v1/quizzes/" + quizID + "/attempts", nil},
		{"read her attempt", http.MethodGet, "/api/v1/quiz-attempts/" + attemptID, nil},
		{"answer in her attempt", http.MethodPost, "/api/v1/quiz-attempts/" + attemptID + "/answers",
			map[string]any{"question_id": questions[0], "selected_index": 0}},
		{"complete her attempt", http.MethodPost, "/api/v1/quiz-attempts/" + attemptID + "/complete", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v",
					tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's list never contains Alice's quizzes, and hers survived all of it
	// -- including her attempt, which must still be open and unanswered.
	if count, _ := getJSON(t, srv, bob, "/api/v1/quizzes")["count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v of Alice's quizzes", count)
	}
	after := getJSON(t, srv, alice, "/api/v1/quiz-attempts/"+attemptID)
	answers, _ := after["answers"].([]any)
	if len(answers) != 0 || after["completed_at"] != nil {
		t.Fatalf("Bob reached Alice's attempt: %v", after)
	}
	if count, _ := getJSON(t, srv, alice, "/api/v1/quizzes")["count"].(float64); count != 1 {
		t.Fatalf("Alice has %v quizzes after Bob's attempts, want 1", count)
	}
}

// A quiz may name a study plan and a document, and both have to be the
// caller's. The answer for somebody else's is the same 404, not a validation
// error naming the field.
func TestCrossUserQuizLinksAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")
	alicePlan := create(t, srv, alice, "/api/v1/study-plans", map[string]any{"title": "Alice's plan"})
	aliceDoc := fmt.Sprint(upload(t, srv, alice, "alice-field-notes.txt", studyDocument)["id"])

	for name, link := range map[string]map[string]any{
		"another user's study plan": {"study_plan_id": alicePlan},
		"another user's document":   {"document_id": aliceDoc},
	} {
		t.Run(name, func(t *testing.T) {
			body := quizBody("Borrowed")
			for k, v := range link {
				body[k] = v
			}
			resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/quizzes", bob, body)
			if resp.StatusCode != http.StatusNotFound || out["error"] != "not_found" {
				t.Fatalf("linking to %s = %d %v, want a 404", name, resp.StatusCode, out)
			}
		})
	}
	if count, _ := getJSON(t, srv, bob, "/api/v1/quizzes")["count"].(float64); count != 0 {
		t.Fatalf("Bob has %v quizzes", count)
	}
}

// The whole lifecycle over HTTP: make a quiz, take it, be graded, finish it,
// and read the score back.
func TestQuizAttemptLifecycleOverHTTP(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	doc := upload(t, srv, alice, "field-notes.txt", studyDocument)
	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{"title": "Field notes revision"})

	body := quizBody("Field notes quiz")
	body["study_plan_id"], body["document_id"] = planID, doc["id"]
	resp, created := doJSON(t, srv, http.MethodPost, "/api/v1/quizzes", alice, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /quizzes: status %d: %v", resp.StatusCode, created)
	}
	quizID := fmt.Sprint(created["id"])
	switch {
	case created["question_count"] != float64(2):
		t.Fatalf("question_count = %v", created["question_count"])
	case created["attempt_count"] != float64(0):
		t.Fatalf("attempt_count = %v", created["attempt_count"])
	case created["best_score"] != nil:
		t.Fatalf("a quiz nobody has taken has a best score of %v", created["best_score"])
	case created["study_plan"] != "Field notes revision":
		t.Fatalf("study_plan = %v, want the title beside the id", created["study_plan"])
	case created["document"] != "field-notes.txt":
		t.Fatalf("document = %v, want the filename beside the id", created["document"])
	}

	// Reading the quiz gives the questions and the options -- and not the
	// answer key. A client rendering this cannot accidentally spoil it.
	quiz := getJSON(t, srv, alice, "/api/v1/quizzes/"+quizID)
	shown, _ := quiz["questions"].([]any)
	if len(shown) != 2 {
		t.Fatalf("the quiz reads back with %d questions", len(shown))
	}
	for _, raw := range shown {
		q, _ := raw.(map[string]any)
		if _, leaked := q["correct_index"]; leaked {
			t.Fatalf("GET /quizzes/{id} hands the client the answer key: %v", q)
		}
		if options, _ := q["options"].([]any); len(options) < 2 {
			t.Fatalf("a question reads back with %d options", len(options))
		}
	}
	questions := quizQuestionIDs(t, quiz)

	// Take it: one right, one wrong.
	attemptID := create(t, srv, alice, "/api/v1/quizzes/"+quizID+"/attempts", nil)
	for _, tc := range []struct {
		question string
		index    int
		correct  bool
	}{
		{questions[0], 0, true},
		{questions[1], 0, false},
	} {
		resp, answer := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/answers",
			alice, map[string]any{"question_id": tc.question, "selected_index": tc.index})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST an answer: status %d: %v", resp.StatusCode, answer)
		}
		if answer["correct"] != tc.correct {
			t.Fatalf("answering %d was marked %v, want %v", tc.index, answer["correct"], tc.correct)
		}
		// The verdict comes with the key: the user learns the answer as soon
		// as they have committed to one.
		if answer["correct_index"] == nil {
			t.Fatalf("the answer does not say which option was right: %v", answer)
		}
	}

	// The same question again is a conflict, not an overwrite.
	resp, body2 := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/answers",
		alice, map[string]any{"question_id": questions[0], "selected_index": 1})
	if resp.StatusCode != http.StatusConflict || body2["error"] != "already_answered" {
		t.Fatalf("answering twice = %d %v, want a 409", resp.StatusCode, body2)
	}

	// Finish it, and the score is the one the answers support.
	resp, done := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/complete", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("completing: %d %v", resp.StatusCode, done)
	}
	switch {
	case done["score"] != float64(1):
		t.Fatalf("score = %v, want 1", done["score"])
	case done["question_count"] != float64(2):
		t.Fatalf("question_count = %v", done["question_count"])
	case done["completed_at"] == nil:
		t.Fatalf("the completed attempt has no completed_at: %v", done)
	}

	// A finished attempt takes no more answers and completes only once.
	resp, body2 = doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/complete", alice, nil)
	if resp.StatusCode != http.StatusConflict || body2["error"] != "attempt_complete" {
		t.Fatalf("completing twice = %d %v, want a 409", resp.StatusCode, body2)
	}
	resp, body2 = doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/answers",
		alice, map[string]any{"question_id": questions[1], "selected_index": 1})
	if resp.StatusCode != http.StatusConflict || body2["error"] != "attempt_complete" {
		t.Fatalf("answering a finished attempt = %d %v, want a 409", resp.StatusCode, body2)
	}

	// The attempt reads back with its answers and its score, and the quiz now
	// reports the attempt and the best score -- both counted on read.
	reread := getJSON(t, srv, alice, "/api/v1/quiz-attempts/"+attemptID)
	if answers, _ := reread["answers"].([]any); len(answers) != 2 {
		t.Fatalf("the attempt reads back with %d answers", len(answers))
	}
	reloaded := getJSON(t, srv, alice, "/api/v1/quizzes/"+quizID)
	if reloaded["attempt_count"] != float64(1) || reloaded["best_score"] != float64(1) {
		t.Fatalf("attempts = %v, best = %v", reloaded["attempt_count"], reloaded["best_score"])
	}

	// Deleting the quiz takes its questions and its attempts with it.
	resp, body2 = doJSON(t, srv, http.MethodDelete, "/api/v1/quizzes/"+quizID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE the quiz: %d %v", resp.StatusCode, body2)
	}
	resp, _ = doJSON(t, srv, http.MethodGet, "/api/v1/quiz-attempts/"+attemptID, alice, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the deleted quiz's attempt answers %d", resp.StatusCode)
	}
}

// A question of another quiz -- including one the caller owns -- is not a
// question in this attempt, and gets the same 404 a stranger's does.
func TestAnAnswerMustNameAQuestionOfThisAttemptsQuiz(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	firstID := create(t, srv, alice, "/api/v1/quizzes", quizBody("First"))
	secondID := create(t, srv, alice, "/api/v1/quizzes", quizBody("Second"))
	second := quizQuestionIDs(t, getJSON(t, srv, alice, "/api/v1/quizzes/"+secondID))
	attemptID := create(t, srv, alice, "/api/v1/quizzes/"+firstID+"/attempts", nil)

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/quiz-attempts/"+attemptID+"/answers",
		alice, map[string]any{"question_id": second[0], "selected_index": 0})
	if resp.StatusCode != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("answering another quiz's question = %d %v, want a 404", resp.StatusCode, body)
	}
}

// Strict decoding and the shared error shape, the same as every other module.
func TestQuizRequestsAreDecodedStrictly(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	quizID := create(t, srv, alice, "/api/v1/quizzes", quizBody("Finals"))
	attemptID := create(t, srv, alice, "/api/v1/quizzes/"+quizID+"/attempts", nil)
	questions := quizQuestionIDs(t, getJSON(t, srv, alice, "/api/v1/quizzes/"+quizID))

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
		status int
		code   string
	}{
		"an unknown field": {http.MethodPost, "/api/v1/quizzes",
			map[string]any{"title": "x", "difficulty": "hard"}, http.StatusBadRequest, "invalid_json"},
		"no title": {http.MethodPost, "/api/v1/quizzes",
			map[string]any{"questions": quizBody("x")["questions"]}, http.StatusBadRequest, "validation_failed"},
		"no questions": {http.MethodPost, "/api/v1/quizzes",
			map[string]any{"title": "Empty"}, http.StatusBadRequest, "validation_failed"},
		"a question with one option": {http.MethodPost, "/api/v1/quizzes",
			map[string]any{"title": "x", "questions": []map[string]any{
				{"question": "q?", "options": []string{"only"}, "correct_index": 0}}},
			http.StatusBadRequest, "validation_failed"},
		"an answer that is not an option": {http.MethodPost, "/api/v1/quizzes",
			map[string]any{"title": "x", "questions": []map[string]any{
				{"question": "q?", "options": []string{"a", "b"}, "correct_index": 9}}},
			http.StatusBadRequest, "validation_failed"},
		"an option that is not one of this question's": {http.MethodPost,
			"/api/v1/quiz-attempts/" + attemptID + "/answers",
			map[string]any{"question_id": questions[0], "selected_index": 99},
			http.StatusBadRequest, "validation_failed"},
		"an answer with no question": {http.MethodPost,
			"/api/v1/quiz-attempts/" + attemptID + "/answers",
			map[string]any{"selected_index": 0}, http.StatusBadRequest, "validation_failed"},
		"a malformed quiz id": {http.MethodGet, "/api/v1/quizzes/not-a-uuid",
			nil, http.StatusNotFound, "not_found"},
		"a malformed attempt id": {http.MethodGet, "/api/v1/quiz-attempts/not-a-uuid",
			nil, http.StatusNotFound, "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, alice, tc.body)
			if resp.StatusCode != tc.status || body["error"] != tc.code {
				t.Fatalf("%s %s = %d %v, want %d %s", tc.method, tc.path,
					resp.StatusCode, body, tc.status, tc.code)
			}
		})
	}

	// And everything needs a token.
	for _, path := range []string{"/api/v1/quizzes", "/api/v1/quizzes/" + quizID,
		"/api/v1/quiz-attempts/" + attemptID} {
		resp, _ := doJSON(t, srv, http.MethodGet, path, "", nil)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("GET %s answered without a token", path)
		}
	}
}

// A quiz gets no knowledge-graph node, and neither does an attempt. The plan
// is the thing worth connecting and the quiz hangs off it, exactly as a deck
// does; see migration 000011.
func TestAQuizGetsNoGraphNode(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{"title": "Field notes revision"})

	body := quizBody("Field notes quiz")
	body["study_plan_id"] = planID
	quizID := create(t, srv, alice, "/api/v1/quizzes", body)
	create(t, srv, alice, "/api/v1/quizzes/"+quizID+"/attempts", nil)

	graph := getJSON(t, srv, alice, "/api/v1/knowledge-graph")
	nodes, _ := graph["nodes"].([]any)
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		switch n["ref_table"] {
		case "quizzes", "quiz_attempts":
			t.Fatalf("a quiz was mirrored into the graph: %v", n)
		}
		if n["ref_id"] == quizID {
			t.Fatalf("a node points at the quiz: %v", n)
		}
	}
	// The plan still has its own, which is what makes this assertion mean
	// "quizzes get none" rather than "the graph is empty".
	found := false
	for _, raw := range nodes {
		if n, _ := raw.(map[string]any); n["ref_table"] == "study_plans" && n["ref_id"] == planID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the plan has no graph node either: %v", graph)
	}
}
