package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Phase 10a half of the property the whole ownership design exists for,
// over the real router, the real service and real SQL.
//
// These are skipped unless TEST_DATABASE_URL is set; see docs/testing.md.

// studyDocument is the text uploaded for these tests. Flashcards made from it
// have to say what it says, which is what the grounding assertion below reads.
const studyDocument = `Field notes, 14 March.

The aurora borealis appeared over the tundra shortly after midnight and lasted
about forty minutes. The dogs slept through the whole thing.

Separately: the generator needs a new fuel filter before the next resupply run.
`

func studyPlans(t *testing.T, srv *httptest.Server, token, query string) map[string]any {
	t.Helper()
	path := "/api/v1/study-plans"
	if query != "" {
		path += "?" + query
	}
	resp, body := doJSON(t, srv, http.MethodGet, path, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %v", path, resp.StatusCode, body)
	}
	return body
}

func flashcards(t *testing.T, srv *httptest.Server, token, planID string) map[string]any {
	t.Helper()
	path := "/api/v1/study-plans/" + planID + "/flashcards"
	resp, body := doJSON(t, srv, http.MethodGet, path, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %v", path, resp.StatusCode, body)
	}
	return body
}

func TestCrossUserStudyPlanIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{
		"title": "Alice's finals", "description": "eigenvalues",
	})
	cardID := create(t, srv, alice, "/api/v1/study-plans/"+planID+"/flashcards", map[string]any{
		"front": "What is an eigenvalue?", "back": "A scalar with Av = λv.",
	})

	// Alice's rows exist and Bob does not own them. The answer must be 404 --
	// a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get plan", http.MethodGet, "/api/v1/study-plans/" + planID, nil},
		{"patch plan", http.MethodPatch, "/api/v1/study-plans/" + planID, map[string]any{"title": "hijacked"}},
		{"complete plan", http.MethodPatch, "/api/v1/study-plans/" + planID, map[string]any{"status": "completed"}},
		{"delete plan", http.MethodDelete, "/api/v1/study-plans/" + planID, nil},
		{"list its cards", http.MethodGet, "/api/v1/study-plans/" + planID + "/flashcards", nil},
		{"add a card to it", http.MethodPost, "/api/v1/study-plans/" + planID + "/flashcards",
			map[string]any{"front": "q?", "back": "a."}},
		{"delete a card", http.MethodDelete, "/api/v1/flashcards/" + cardID, nil},
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

	// Bob's list never contains Alice's plans, and hers survived all of it.
	if count, _ := studyPlans(t, srv, bob, "")["count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v of Alice's plans", count)
	}
	if count, _ := studyPlans(t, srv, alice, "")["count"].(float64); count != 1 {
		t.Fatalf("Alice has %v plans after Bob's attempts, want 1", count)
	}
	if count, _ := flashcards(t, srv, alice, planID)["count"].(float64); count != 1 {
		t.Fatalf("Alice has %v cards after Bob's attempts, want 1", count)
	}
}

// A plan can name a document, and it has to be the caller's. The answer for
// somebody else's is the same 404, not a validation error naming the field.
func TestCrossUserStudyLinksAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")
	aliceDoc := fmt.Sprint(upload(t, srv, alice, "alice-field-notes.txt", studyDocument)["id"])

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/study-plans", bob, map[string]any{
		"title": "Borrowed", "document_id": aliceDoc,
	})
	if resp.StatusCode != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("linking to another user's document = %d %v, want a 404", resp.StatusCode, body)
	}

	// And the same on a PATCH, which is the other door to the column.
	bobPlan := create(t, srv, bob, "/api/v1/study-plans", map[string]any{"title": "Bob's plan"})
	resp, body = doJSON(t, srv, http.MethodPatch, "/api/v1/study-plans/"+bobPlan, bob, map[string]any{
		"document_id": aliceDoc,
	})
	if resp.StatusCode != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("patching in another user's document = %d %v, want a 404", resp.StatusCode, body)
	}
	if count, _ := studyPlans(t, srv, bob, "")["count"].(float64); count != 1 {
		t.Fatalf("Bob has %v plans", count)
	}
}

func TestStudyPlanLifecycleOverHTTP(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	doc := upload(t, srv, alice, "field-notes.txt", studyDocument)

	resp, created := doJSON(t, srv, http.MethodPost, "/api/v1/study-plans", alice, map[string]any{
		"title": "Field notes revision", "document_id": doc["id"],
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /study-plans: status %d: %v", resp.StatusCode, created)
	}
	planID := fmt.Sprint(created["id"])
	switch {
	case created["status"] != "active":
		t.Fatalf("a new plan is %v, want active", created["status"])
	case created["document"] != "field-notes.txt":
		t.Fatalf("document = %v, want the filename beside the id", created["document"])
	case created["card_count"] != float64(0):
		t.Fatalf("card_count = %v", created["card_count"])
	}

	// Two cards by hand. They carry no source document: the user wrote them.
	for _, card := range []map[string]any{
		{"front": "How long did the aurora last?", "back": "About forty minutes."},
		{"front": "What does the generator need?", "back": "A new fuel filter."},
	} {
		resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/study-plans/"+planID+"/flashcards", alice, card)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("POST a flashcard: status %d: %v", resp.StatusCode, body)
		}
		if body["document_id"] != nil {
			t.Fatalf("a hand-written card claims a source: %v", body["document_id"])
		}
	}

	deck := flashcards(t, srv, alice, planID)
	if count, _ := deck["count"].(float64); count != 2 {
		t.Fatalf("the deck has %v cards, want 2", count)
	}
	// The count on the plan is computed, so it cannot disagree with the deck.
	resp, reloaded := doJSON(t, srv, http.MethodGet, "/api/v1/study-plans/"+planID, alice, nil)
	if resp.StatusCode != http.StatusOK || reloaded["card_count"] != float64(2) {
		t.Fatalf("card_count = %v", reloaded["card_count"])
	}

	// A card can be deleted on its own, at the top level -- a card may belong
	// to no plan, so it cannot be addressed only as a child of one.
	list, _ := deck["flashcards"].([]any)
	first, _ := list[0].(map[string]any)
	resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/flashcards/"+fmt.Sprint(first["id"]), alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE a flashcard: status %d: %v", resp.StatusCode, body)
	}
	if count, _ := flashcards(t, srv, alice, planID)["count"].(float64); count != 1 {
		t.Fatalf("the deck still has %v cards", count)
	}

	// Deleting the plan takes the rest of the deck with it.
	resp, body = doJSON(t, srv, http.MethodDelete, "/api/v1/study-plans/"+planID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE the plan: status %d: %v", resp.StatusCode, body)
	}
	resp, _ = doJSON(t, srv, http.MethodGet, "/api/v1/study-plans/"+planID+"/flashcards", alice, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the deleted plan's deck answers %d", resp.StatusCode)
	}
}

// Strict decoding and the shared error shape, the same as every other module.
func TestStudyRequestsAreDecodedStrictly(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{"title": "Finals"})

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
		status int
		code   string
	}{
		"an unknown field": {http.MethodPost, "/api/v1/study-plans",
			map[string]any{"title": "x", "cards": 5}, http.StatusBadRequest, "invalid_json"},
		"no title": {http.MethodPost, "/api/v1/study-plans",
			map[string]any{"description": "x"}, http.StatusBadRequest, "validation_failed"},
		"an unknown status": {http.MethodPost, "/api/v1/study-plans",
			map[string]any{"title": "x", "status": "studying"}, http.StatusBadRequest, "validation_failed"},
		"a null title": {http.MethodPatch, "/api/v1/study-plans/" + planID,
			map[string]any{"title": nil}, http.StatusBadRequest, "validation_failed"},
		"a card with no back": {http.MethodPost, "/api/v1/study-plans/" + planID + "/flashcards",
			map[string]any{"front": "q?"}, http.StatusBadRequest, "validation_failed"},
		"a malformed plan id": {http.MethodGet, "/api/v1/study-plans/not-a-uuid",
			nil, http.StatusNotFound, "not_found"},
		"a malformed card id": {http.MethodDelete, "/api/v1/flashcards/not-a-uuid",
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
	for _, path := range []string{"/api/v1/study-plans", "/api/v1/flashcards/" + planID} {
		resp, _ := doJSON(t, srv, http.MethodGet, path, "", nil)
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("GET %s answered without a token", path)
		}
	}
}

func TestAStudyPlanGetsAGraphNode(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	planID := create(t, srv, alice, "/api/v1/study-plans", map[string]any{
		"title": "Linear algebra finals",
	})

	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=project", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /knowledge-graph: status %d: %v", resp.StatusCode, body)
	}
	nodes, _ := body["nodes"].([]any)
	var found map[string]any
	for _, raw := range nodes {
		n, _ := raw.(map[string]any)
		if n["ref_table"] == "study_plans" && n["ref_id"] == planID {
			found = n
		}
	}
	if found == nil {
		t.Fatalf("the plan has no graph node: %v", body)
	}
	if found["label"] != "Linear algebra finals" {
		t.Fatalf("label = %v, want the plan's title", found["label"])
	}
	// A mirrored node cannot be deleted directly, and that has to hold for a
	// `project` -- the one type that is both mirrored and extractable.
	resp, body = doJSON(t, srv, http.MethodDelete,
		"/api/v1/knowledge-graph/nodes/"+fmt.Sprint(found["id"]), alice, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a mirrored project node = %d %v, want a 409", resp.StatusCode, body)
	}

	// Deleting the plan takes the node, through the trigger.
	if resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/study-plans/"+planID, alice, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE the plan: %d %v", resp.StatusCode, body)
	}
	_, body = doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=project", alice, nil)
	nodes, _ = body["nodes"].([]any)
	for _, raw := range nodes {
		if n, _ := raw.(map[string]any); n["ref_id"] == planID {
			t.Fatalf("the node outlived the plan: %v", n)
		}
	}
}
