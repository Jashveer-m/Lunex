package api_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/db"
	"github.com/jashveer/lifeos/backend/migrations"
)

// These tests run the whole stack — router, RequireAuth, services, real SQL —
// against a throwaway Postgres. They are skipped unless TEST_DATABASE_URL is
// set; see docs/testing.md.
//
// They exist to pin one property: a user cannot read, modify or delete another
// user's rows, and the API answers 404 rather than 403 so it never confirms
// that somebody else's id exists.
func isolationServer(t *testing.T) *httptest.Server {
	return isolationServerWithProvider(t, &ai.Mock{})
}

// isolationServerWithProvider is the same stack with a chosen model, for the
// Phase 4 tests that care what the assistant answers.
func isolationServerWithProvider(t *testing.T, provider *ai.Mock) *httptest.Server {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database integration tests")
	}
	pool, err := db.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Up(pool, migrations.FS); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Tasks, goals, notes, documents, chunks, conversations and messages all
	// cascade from users, so one truncate is enough.
	if _, err := pool.Exec(`TRUNCATE users CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return newServerWithProvider(t, pool, provider)
}

// register creates a user through the API and returns their access token.
func register(t *testing.T, srv *httptest.Server, email string) string {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": email, "password": "correct horse battery staple", "name": email,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status %d: %v", email, resp.StatusCode, body)
	}
	tokens, _ := body["tokens"].(map[string]any)
	access, _ := tokens["access_token"].(string)
	if access == "" {
		t.Fatalf("register %s returned no access token: %v", email, body)
	}
	return access
}

// create posts a resource and returns its id.
func create(t *testing.T, srv *httptest.Server, token, path string, body any) string {
	t.Helper()
	resp, out := doJSON(t, srv, http.MethodPost, path, token, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: status %d: %v", path, resp.StatusCode, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("POST %s returned no id: %v", path, out)
	}
	return id
}

// TestCrossUserIsolation is the test the whole ownership design exists for.
func TestCrossUserIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	taskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Alice's task"})
	otherTaskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Alice's other task"})
	goalID := create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "Alice's goal", "type": "career"})
	noteID := create(t, srv, alice, "/api/v1/notes", map[string]any{"title": "Alice's note"})
	milestoneID := create(t, srv, alice, "/api/v1/goals/"+goalID+"/milestones", map[string]any{"title": "Ship it"})

	// Every one of these is a resource that exists and that Bob does not own.
	// The answer must be 404 — a 403 would confirm the id is real.
	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get task", http.MethodGet, "/api/v1/tasks/" + taskID, nil},
		{"patch task", http.MethodPatch, "/api/v1/tasks/" + taskID, map[string]any{"title": "hijacked"}},
		{"delete task", http.MethodDelete, "/api/v1/tasks/" + taskID, nil},
		{"depend on task", http.MethodPost, "/api/v1/tasks/" + taskID + "/dependencies", map[string]any{"depends_on_task_id": otherTaskID}},
		{"get goal", http.MethodGet, "/api/v1/goals/" + goalID, nil},
		{"patch goal", http.MethodPatch, "/api/v1/goals/" + goalID, map[string]any{"title": "hijacked"}},
		{"delete goal", http.MethodDelete, "/api/v1/goals/" + goalID, nil},
		{"add milestone", http.MethodPost, "/api/v1/goals/" + goalID + "/milestones", map[string]any{"title": "hijacked"}},
		{"patch milestone", http.MethodPatch, "/api/v1/goals/" + goalID + "/milestones/" + milestoneID, map[string]any{"completed": true}},
		{"get note", http.MethodGet, "/api/v1/notes/" + noteID, nil},
		{"patch note", http.MethodPatch, "/api/v1/notes/" + noteID, map[string]any{"title": "hijacked"}},
		{"delete note", http.MethodDelete, "/api/v1/notes/" + noteID, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's lists never contain Alice's rows.
	for _, path := range []string{"/api/v1/tasks", "/api/v1/goals", "/api/v1/notes"} {
		resp, body := doJSON(t, srv, http.MethodGet, path, bob, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d: %v", path, resp.StatusCode, body)
		}
		if count, _ := body["count"].(float64); count != 0 {
			t.Fatalf("GET %s as the other user returned %v rows, want 0: %v", path, count, body)
		}
	}

	// And none of that touched Alice's data: every attempt above must have
	// been a no-op, not just an unhelpful response code.
	resp, task := doJSON(t, srv, http.MethodGet, "/api/v1/tasks/"+taskID, alice, nil)
	if resp.StatusCode != http.StatusOK || task["title"] != "Alice's task" {
		t.Fatalf("Alice's task after Bob's attempts = %d %v", resp.StatusCode, task)
	}
	resp, note := doJSON(t, srv, http.MethodGet, "/api/v1/notes/"+noteID, alice, nil)
	if resp.StatusCode != http.StatusOK || note["title"] != "Alice's note" {
		t.Fatalf("Alice's note after Bob's attempts = %d %v", resp.StatusCode, note)
	}
	resp, goal := doJSON(t, srv, http.MethodGet, "/api/v1/goals/"+goalID, alice, nil)
	if resp.StatusCode != http.StatusOK || goal["title"] != "Alice's goal" {
		t.Fatalf("Alice's goal after Bob's attempts = %d %v", resp.StatusCode, goal)
	}
	milestones, _ := goal["milestones"].([]any)
	if len(milestones) != 1 {
		t.Fatalf("Alice's goal has %d milestones, want 1 (Bob must not have added one)", len(milestones))
	}
	if first, _ := milestones[0].(map[string]any); first["completed"] != false {
		t.Fatalf("Bob completed Alice's milestone: %v", first)
	}
}

// A task may not depend on, or be parented by, another user's task. Both would
// leak the existence of a foreign id through an otherwise-successful write.
func TestCrossUserTaskLinksAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	aliceTask := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Alice's task"})
	bobTask := create(t, srv, bob, "/api/v1/tasks", map[string]any{"title": "Bob's task"})

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/tasks/"+bobTask+"/dependencies", bob,
		map[string]any{"depends_on_task_id": aliceTask})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("depending on another user's task = %d, want 404: %v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, srv, http.MethodPost, "/api/v1/tasks", bob,
		map[string]any{"title": "child", "parent_task_id": aliceTask})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("parenting to another user's task = %d, want 404: %v", resp.StatusCode, body)
	}

	resp, body = doJSON(t, srv, http.MethodPatch, "/api/v1/tasks/"+bobTask, bob,
		map[string]any{"parent_task_id": aliceTask})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-parenting to another user's task = %d, want 404: %v", resp.StatusCode, body)
	}
}

// Deleting a user must take their Phase 2 rows with them.
func TestUserDeletionCascadesToPhase2Rows(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	goalID := create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "g", "type": "personal"})
	create(t, srv, alice, "/api/v1/goals/"+goalID+"/milestones", map[string]any{"title": "m"})
	create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "t"})
	create(t, srv, alice, "/api/v1/notes", map[string]any{"title": "n"})

	pool := mustPool(t)
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tasks", "goals", "goal_milestones", "notes"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s after the user was deleted", n, table)
		}
	}
}

func mustPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(context.Background(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

// --- Phase 3: documents -----------------------------------------------------

// upload posts a multipart file and returns the created document.
func upload(t *testing.T, srv *httptest.Server, token, filename, body string) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/documents", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /documents %s: status %d: %v", filename, resp.StatusCode, out)
	}
	if out["status"] != "ready" {
		t.Fatalf("document %s is %v, want ready: %v", filename, out["status"], out["error_message"])
	}
	return out
}

// The Phase 3 half of the property the whole ownership design exists for.
func TestCrossUserDocumentIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	const secret = "The launch code for the Aurora satellite is quetzal seventeen."
	aliceDoc := upload(t, srv, alice, "alice-secrets.txt", secret)
	aliceID, _ := aliceDoc["id"].(string)
	upload(t, srv, bob, "bob-notes.txt", "Bob keeps a list of birds he has seen.")

	// Alice's document exists and Bob does not own it. The answer must be 404
	// — a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"get document", http.MethodGet, "/api/v1/documents/" + aliceID},
		{"delete document", http.MethodDelete, "/api/v1/documents/" + aliceID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, nil)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's list holds only his own document.
	resp, list := doJSON(t, srv, http.MethodGet, "/api/v1/documents", bob, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /documents: %d %v", resp.StatusCode, list)
	}
	if count, _ := list["count"].(float64); count != 1 {
		t.Fatalf("Bob sees %v documents, want only his own: %v", count, list)
	}

	// The retrieval path is the one that could leak text without ever
	// mentioning an id: Bob searches for the exact words in Alice's file.
	resp, found := doJSON(t, srv, http.MethodPost, "/api/v1/documents/search", bob,
		map[string]any{"query": "Aurora satellite launch code quetzal", "limit": 20})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /documents/search: %d %v", resp.StatusCode, found)
	}
	results, _ := found["results"].([]any)
	for _, raw := range results {
		r, _ := raw.(map[string]any)
		content, _ := r["content"].(string)
		filename, _ := r["filename"].(string)
		if strings.Contains(content, "quetzal") || strings.Contains(filename, "alice") {
			t.Fatalf("Bob's search returned Alice's content: %v", r)
		}
	}

	// Naming Alice's document id explicitly must not reach it either.
	resp, found = doJSON(t, srv, http.MethodPost, "/api/v1/documents/search", bob,
		map[string]any{"query": "Aurora satellite", "document_ids": []string{aliceID}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /documents/search with a foreign id: %d %v", resp.StatusCode, found)
	}
	if count, _ := found["count"].(float64); count != 0 {
		t.Fatalf("searching another user's document id returned %v results, want 0: %v", count, found)
	}

	// And none of that touched Alice's data.
	resp, doc := doJSON(t, srv, http.MethodGet, "/api/v1/documents/"+aliceID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Alice's document after Bob's attempts = %d %v", resp.StatusCode, doc)
	}
	if text, _ := doc["extracted_text"].(string); text != secret {
		t.Fatalf("Alice's extracted text = %q, want it unchanged", text)
	}
}

// Retrieval works for the owner: the point of the module, and the control
// that makes the isolation assertions above meaningful rather than vacuous.
func TestSearchFindsTheOwnersChunk(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	doc := upload(t, srv, alice, "field-notes.md",
		"# Field notes\n\nThe aurora borealis appeared over the tundra shortly after midnight.\n")
	if chunks, _ := doc["chunk_count"].(float64); chunks != 1 {
		t.Fatalf("chunk_count = %v, want 1", chunks)
	}

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/documents/search", alice,
		map[string]any{"query": "aurora borealis tundra", "limit": 5})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d %v", resp.StatusCode, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %v", len(results), body)
	}

	first, _ := results[0].(map[string]any)
	if content, _ := first["content"].(string); !strings.Contains(content, "aurora borealis") {
		t.Fatalf("content = %q, want the chunk from the file", content)
	}
	// The citation: which document, which chunk, and how close.
	if first["filename"] != "field-notes.md" {
		t.Fatalf("filename = %v, want field-notes.md", first["filename"])
	}
	if first["document_id"] != doc["id"] {
		t.Fatalf("document_id = %v, want %v", first["document_id"], doc["id"])
	}
	if sim, _ := first["similarity"].(float64); sim <= 0 || sim > 1.0000001 {
		t.Fatalf("similarity = %v, want a positive score no greater than 1", sim)
	}

	// A question about something else must not come back with a confident
	// citation from this document.
	resp, body = doJSON(t, srv, http.MethodPost, "/api/v1/documents/search", alice,
		map[string]any{"query": "quarterly revenue forecast", "min_similarity": 0.5})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d %v", resp.StatusCode, body)
	}
	if count, _ := body["count"].(float64); count != 0 {
		t.Fatalf("an unrelated query returned %v results above the floor: %v", count, body)
	}
}

// Deleting a document takes its chunks with it, so the text stops being
// retrievable rather than merely stops being listed.
func TestDeletingADocumentRemovesItFromSearch(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	doc := upload(t, srv, alice, "notes.txt", "The aurora borealis appeared over the tundra.")
	id, _ := doc["id"].(string)

	resp, _ := doJSON(t, srv, http.MethodDelete, "/api/v1/documents/"+id, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/documents/search", alice,
		map[string]any{"query": "aurora borealis tundra"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search: %d %v", resp.StatusCode, body)
	}
	if count, _ := body["count"].(float64); count != 0 {
		t.Fatalf("search returned %v results after the document was deleted: %v", count, body)
	}

	pool := mustPool(t)
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM document_chunks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d chunks survived the deleted document", n)
	}
}

// A file the phase cannot read is rejected before a row exists, not stored as
// a document that failed.
func TestUnsupportedFileTypesAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	for _, tc := range []struct{ name, body string }{
		{"report.docx", "PK\x03\x04 not really a docx"},
		{"rows.csv", "a,b\n1,2\n"},
		{"scan.png", "\x89PNG\r\n\x1a\n"},
		{"claims-to-be.pdf", "this is not a pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			part, _ := mw.CreateFormFile("file", tc.name)
			_, _ = part.Write([]byte(tc.body))
			_ = mw.Close()

			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/documents", &buf)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.Header.Set("Authorization", "Bearer "+alice)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415", resp.StatusCode)
			}
		})
	}

	// No half-made rows behind those rejections.
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/documents", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if count, _ := body["count"].(float64); count != 0 {
		t.Fatalf("%v documents exist after four rejected uploads, want 0", count)
	}
}

// --- Phase 4: the AI assistant ----------------------------------------------

// sseEvent is one parsed Server-Sent Event from a chat stream.
type sseEvent struct {
	Name string
	Data map[string]any
}

// ask posts a message to a conversation and returns the whole stream. The
// model is a mock, so nothing here waits on Ollama.
func ask(t *testing.T, srv *httptest.Server, token, convID, content string) (*http.Response, []sseEvent) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/conversations/"+convID+"/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp, nil
	}

	var events []sseEvent
	var name string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var data map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
				t.Fatalf("decode %s event: %v", name, err)
			}
			events = append(events, sseEvent{Name: name, Data: data})
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return resp, events
}

// answer returns the reassembled reply and the done event of a stream.
func answer(t *testing.T, events []sseEvent) (string, map[string]any) {
	t.Helper()
	var text strings.Builder
	var done map[string]any
	for _, e := range events {
		switch e.Name {
		case "token":
			s, _ := e.Data["text"].(string)
			text.WriteString(s)
		case "done":
			done = e.Data
		case "error":
			t.Fatalf("stream failed: %v", e.Data)
		}
	}
	if done == nil {
		t.Fatalf("stream had no done event: %v", events)
	}
	return text.String(), done
}

// citingProvider answers by citing whatever the retrieved context contains,
// which is what makes the grounding assertions below meaningful: the reply
// tracks the prompt rather than being a fixed string.
func citingProvider() *ai.Mock {
	return &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		// Keyed on a retrieved source header, not on the words of the
		// question: the point is to answer from the context or say it is not
		// there, and the question itself mentions the aurora either way.
		prompt := ai.PromptText(msgs)
		if strings.Contains(prompt, "[S1] document:") {
			return "Your notes say the aurora appeared over the tundra [S1]."
		}
		return "I could not find anything about that in your documents, tasks, goals or notes."
	}}
}

// The Phase 4 half of the property the whole ownership design exists for.
func TestCrossUserConversationIsolation(t *testing.T) {
	srv := isolationServerWithProvider(t, citingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{"title": "Alice's chat"})
	if _, events := ask(t, srv, alice, convID, "hello"); len(events) == 0 {
		t.Fatal("Alice's own message produced no stream")
	}

	// Alice's conversation exists and Bob does not own it. The answer must be
	// 404 — a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get conversation", http.MethodGet, "/api/v1/conversations/" + convID, nil},
		{"delete conversation", http.MethodDelete, "/api/v1/conversations/" + convID, nil},
		{"send a message", http.MethodPost, "/api/v1/conversations/" + convID + "/messages", map[string]any{"content": "hijacked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's list never contains Alice's conversation.
	resp, list := doJSON(t, srv, http.MethodGet, "/api/v1/conversations", bob, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /conversations: %d %v", resp.StatusCode, list)
	}
	if count, _ := list["count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v conversations, want 0: %v", count, list)
	}

	// And none of that touched Alice's conversation: the attempts must have
	// been no-ops, not just unhelpful status codes.
	resp, conv := doJSON(t, srv, http.MethodGet, "/api/v1/conversations/"+convID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Alice's conversation after Bob's attempts = %d %v", resp.StatusCode, conv)
	}
	msgs, _ := conv["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("Alice's conversation holds %d messages, want the one turn she sent: %v", len(msgs), conv)
	}
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if content, _ := m["content"].(string); strings.Contains(content, "hijacked") {
			t.Fatalf("Bob's message landed in Alice's conversation: %v", m)
		}
	}
}

// The retrieval path is the one that could leak another user's text without
// ever naming an id: Bob asks the assistant the exact words in Alice's file.
func TestChatRetrievalCannotReachAnotherUsersDocuments(t *testing.T) {
	srv := isolationServerWithProvider(t, citingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	const secret = "The aurora borealis appeared over the tundra shortly after midnight."
	upload(t, srv, alice, "alice-field-notes.md", secret)

	convID := create(t, srv, bob, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, bob, convID, "What do my notes say about the aurora borealis over the tundra?")
	text, done := answer(t, events)

	message, _ := done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	for _, raw := range sources {
		s, _ := raw.(map[string]any)
		if title, _ := s["title"].(string); strings.Contains(title, "alice") {
			t.Fatalf("Bob's assistant cited Alice's document: %v", s)
		}
	}
	if len(sources) != 0 {
		t.Fatalf("Bob retrieved %d sources from an empty corpus: %v", len(sources), sources)
	}
	if strings.Contains(text, "aurora appeared") {
		t.Fatalf("Bob's assistant answered from Alice's document: %q", text)
	}
	if !strings.Contains(text, "could not find") {
		t.Fatalf("answer = %q, want it to say nothing was found", text)
	}
}

// The control that makes the isolation assertions above non-vacuous, and the
// grounding rule end to end: the owner's own question retrieves the document
// and the answer cites it, while an unrelated question cites nothing.
func TestChatGroundsAnswersInTheOwnersDataOnly(t *testing.T) {
	srv := isolationServerWithProvider(t, citingProvider())
	alice := register(t, srv, "alice@example.com")

	doc := upload(t, srv, alice, "field-notes.md",
		"The aurora borealis appeared over the tundra shortly after midnight.")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})

	// A question the document answers.
	_, events := ask(t, srv, alice, convID, "What do my notes say about the aurora borealis over the tundra?")
	if events[0].Name != "sources" {
		t.Fatalf("first event = %q, want sources", events[0].Name)
	}
	text, done := answer(t, events)
	message, _ := done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("retrieved %d sources, want the one document: %v", len(sources), sources)
	}
	cited, _ := sources[0].(map[string]any)
	if cited["type"] != "document" || cited["title"] != "field-notes.md" || cited["id"] != doc["id"] {
		t.Fatalf("source = %v, want the uploaded document", cited)
	}
	if cited["cited"] != true {
		t.Fatalf("the answer cited %v but the record says otherwise: %v", cited["label"], cited)
	}
	if !strings.Contains(text, "[S1]") {
		t.Fatalf("answer = %q, want it to carry the citation", text)
	}

	// A question about something else. Nothing is retrieved above the
	// similarity floor, so there is nothing to cite — and the answer says so
	// rather than reaching for the document that is there.
	_, events = ask(t, srv, alice, convID, "quarterly revenue forecast spreadsheet")
	text, done = answer(t, events)
	message, _ = done["message"].(map[string]any)
	sources, _ = message["sources"].([]any)
	if len(sources) != 0 {
		t.Fatalf("an unrelated question retrieved %d sources: %v", len(sources), sources)
	}
	if strings.Contains(text, "[S") || strings.Contains(text, "aurora") {
		t.Fatalf("an unrelated question produced a citation: %q", text)
	}

	// Both turns are on the record, in order, and the conversation took its
	// name from the first question.
	resp, conv := doJSON(t, srv, http.MethodGet, "/api/v1/conversations/"+convID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	msgs, _ := conv["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("conversation holds %d messages, want 4", len(msgs))
	}
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m["role"] != wantRoles[i] {
			t.Fatalf("message %d is %v, want %s — the answer must not precede the question", i, m["role"], wantRoles[i])
		}
	}
	if title, _ := conv["title"].(string); !strings.HasPrefix(title, "What do my notes say") {
		t.Fatalf("title = %q, want it derived from the first question", title)
	}
}

// A failed turn leaves the conversation exactly as it was: no dangling
// question, no half-written answer.
func TestAFailedTurnIsNotPersisted(t *testing.T) {
	srv := isolationServerWithProvider(t, &ai.Mock{Err: ai.ErrUnavailable})
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})

	resp, body := doJSON(t, srv, http.MethodPost, "/api/v1/conversations/"+convID+"/messages",
		alice, map[string]any{"content": "hello"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", resp.StatusCode, body)
	}
	if body["error"] != "model_unavailable" {
		t.Fatalf("error = %v, want model_unavailable", body["error"])
	}

	resp, conv := doJSON(t, srv, http.MethodGet, "/api/v1/conversations/"+convID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if msgs, _ := conv["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("a failed turn left %d messages behind: %v", len(msgs), msgs)
	}
	if title, _ := conv["title"].(string); title != "New conversation" {
		t.Fatalf("a failed turn renamed the conversation to %q", title)
	}
}

// Deleting a conversation takes its messages with it, and deleting a user
// takes their conversations.
func TestConversationAndUserDeletionCascade(t *testing.T) {
	srv := isolationServerWithProvider(t, citingProvider())
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	ask(t, srv, alice, convID, "hello")

	pool := mustPool(t)
	var messages int
	if err := pool.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 2 {
		t.Fatalf("%d messages stored, want 2", messages)
	}

	resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/conversations/"+convID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %v", resp.StatusCode, body)
	}
	if err := pool.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("%d messages survived the conversation, want 0", messages)
	}

	// And a second conversation goes with the user.
	second := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	ask(t, srv, alice, second, "hello again")
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"conversations", "messages"} {
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows survived in %s after the user was deleted", n, table)
		}
	}
}

// --- Phase 5: the memory system ---------------------------------------------

// The fact used throughout the memory tests, and the question that should
// retrieve it. Both are phrased to overlap lexically because hashingEmbedder is
// a bag of words; against a real embedding model the question would not have to
// echo the fact. See embedder_test.go.
const (
	rememberedFact  = "The user prefers studying in the morning."
	recallQuestion  = "When do I prefer studying in the morning?"
	statingTheFact  = "I prefer studying in the morning before class, so please keep my revision blocks early in the day."
	unrelatedAsking = "quarterly revenue forecast spreadsheet"
)

// rememberingProvider is a mock that plays both parts: it answers an extraction
// call with the JSON the prompt asks for, and an ordinary chat turn by citing
// whatever memory it was given. One provider serves both because the service
// wiring does -- and keeping them in one function is what makes it obvious the
// two calls are told apart by their prompts, not by their order.
func rememberingProvider() *ai.Mock {
	return &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		prompt := ai.PromptText(msgs)
		switch {
		case strings.Contains(prompt, "Return the JSON array now."):
			return `[{"type":"preference","content":"` + rememberedFact +
				`","importance":0.8,"confidence":0.9}]`
		case strings.Contains(prompt, "[S1] memory:"):
			return "You told me before that you prefer studying in the morning [S1]."
		default:
			return "I could not find anything about that in your documents, tasks, goals or notes."
		}
	}}
}

// listMemories reads the caller's memories through the API.
func listMemories(t *testing.T, srv *httptest.Server, token, query string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/memories"+query, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /memories%s: status %d: %v", query, resp.StatusCode, body)
	}
	return body
}

// remember runs one conversation that states a durable fact, and returns the
// memory the assistant extracted from it.
func remember(t *testing.T, srv *httptest.Server, token string) map[string]any {
	t.Helper()
	convID := create(t, srv, token, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, token, convID, statingTheFact)
	if _, done := answer(t, events); done == nil {
		t.Fatal("the stating turn produced no done event")
	}

	list := listMemories(t, srv, token, "")
	items, _ := list["memories"].([]any)
	if len(items) != 1 {
		t.Fatalf("the exchange produced %d memories, want 1: %v", len(items), list)
	}
	m, _ := items[0].(map[string]any)
	return m
}

// The end-to-end property Phase 5 exists for, over HTTP: a fact stated in one
// conversation is extracted, and a *different* conversation retrieves it, uses
// it and cites it.
func TestAFactLearnedInOneConversationIsUsedInAnother(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")

	stored := remember(t, srv, alice)
	if stored["content"] != rememberedFact {
		t.Fatalf("content = %v, want the extracted fact", stored["content"])
	}
	if stored["type"] != "preference" {
		t.Fatalf("type = %v, want preference", stored["type"])
	}
	if stored["enabled"] != true {
		t.Fatalf("a new memory is not enabled: %v", stored)
	}
	// Provenance: the memory says which conversation taught it.
	if stored["source_conversation_id"] == nil {
		t.Fatalf("the memory has no source conversation: %v", stored)
	}
	if imp, _ := stored["importance"].(float64); imp != 0.8 {
		t.Fatalf("importance = %v, want the extracted 0.8", stored["importance"])
	}

	// A new conversation: no shared history, nothing but the memory.
	second := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, second, recallQuestion)
	text, done := answer(t, events)

	message, _ := done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("retrieved %d sources, want the one memory: %v", len(sources), sources)
	}
	cited, _ := sources[0].(map[string]any)
	switch {
	case cited["type"] != "memory":
		t.Fatalf("source type = %v, want memory", cited["type"])
	case cited["id"] != stored["id"]:
		t.Fatalf("source id = %v, want the stored memory %v", cited["id"], stored["id"])
	case cited["title"] != "preference":
		t.Fatalf("source title = %v, want the memory's type", cited["title"])
	case cited["cited"] != true:
		t.Fatalf("the answer cited the memory but the record says otherwise: %v", cited)
	}
	if !strings.Contains(text, "[S1]") {
		t.Fatalf("answer = %q, want it to carry the citation", text)
	}

	// And a question about something else retrieves nothing: the floor is what
	// keeps a standing fact about the user out of every unrelated answer.
	_, events = ask(t, srv, alice, second, unrelatedAsking)
	text, done = answer(t, events)
	message, _ = done["message"].(map[string]any)
	if sources, _ = message["sources"].([]any); len(sources) != 0 {
		t.Fatalf("an unrelated question retrieved %v", sources)
	}
	if strings.Contains(text, "[S") {
		t.Fatalf("an unrelated question produced a citation: %q", text)
	}
}

// The Phase 5 half of the property the whole ownership design exists for.
func TestCrossUserMemoryIsolation(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	stored := remember(t, srv, alice)
	aliceMemoryID, _ := stored["id"].(string)

	// Alice's memory exists and Bob does not own it. The answer must be 404 --
	// a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"patch memory", http.MethodPatch, "/api/v1/memories/" + aliceMemoryID, map[string]any{"content": "hijacked"}},
		{"disable memory", http.MethodPatch, "/api/v1/memories/" + aliceMemoryID, map[string]any{"enabled": false}},
		{"delete memory", http.MethodDelete, "/api/v1/memories/" + aliceMemoryID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, srv, tc.method, tc.path, bob, tc.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s as the wrong user = %d, want 404: %v", tc.method, tc.path, resp.StatusCode, body)
			}
			if body["error"] != "not_found" {
				t.Fatalf("error = %v, want not_found", body["error"])
			}
		})
	}

	// Bob's list never contains Alice's memory.
	list := listMemories(t, srv, bob, "")
	if count, _ := list["count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v memories, want 0: %v", count, list)
	}

	// Bob forgetting everything forgets only his own.
	resp, cleared := doJSON(t, srv, http.MethodDelete, "/api/v1/memories", bob, map[string]any{"confirm": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bob's clear = %d: %v", resp.StatusCode, cleared)
	}
	if deleted, _ := cleared["deleted"].(float64); deleted != 0 {
		t.Fatalf("Bob's clear deleted %v memories, want 0", deleted)
	}

	// And none of that touched Alice's memory: every attempt was a no-op, not
	// just an unhelpful status code.
	list = listMemories(t, srv, alice, "")
	items, _ := list["memories"].([]any)
	if len(items) != 1 {
		t.Fatalf("Alice has %d memories after Bob's attempts, want 1: %v", len(items), list)
	}
	survivor, _ := items[0].(map[string]any)
	if survivor["content"] != rememberedFact || survivor["enabled"] != true {
		t.Fatalf("Alice's memory after Bob's attempts = %v", survivor)
	}
}

// The retrieval path is the one that could leak another user's memory without
// ever naming an id: Bob asks the assistant the question that retrieves it.
func TestChatRetrievalCannotReachAnotherUsersMemories(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	remember(t, srv, alice)

	convID := create(t, srv, bob, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, bob, convID, recallQuestion)
	text, done := answer(t, events)

	message, _ := done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	if len(sources) != 0 {
		t.Fatalf("Bob retrieved %d sources from an empty memory: %v", len(sources), sources)
	}
	if strings.Contains(text, "You told me before") {
		t.Fatalf("Bob's assistant answered from Alice's memory: %q", text)
	}
	if !strings.Contains(text, "could not find") {
		t.Fatalf("answer = %q, want it to say nothing was found", text)
	}
	// Bob's own memory is still empty afterwards: the question was short and
	// nothing durable was in it.
	if count, _ := listMemories(t, srv, bob, "")["count"].(float64); count != 0 {
		t.Fatalf("Bob's memory holds %v facts", count)
	}
}

// A disabled memory stops reaching the model but stays on the user's list --
// the difference between "stop using this" and "delete this".
func TestDisablingAMemoryRemovesItFromRetrievalOnly(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")

	stored := remember(t, srv, alice)
	id, _ := stored["id"].(string)

	resp, patched := doJSON(t, srv, http.MethodPatch, "/api/v1/memories/"+id, alice,
		map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %v", resp.StatusCode, patched)
	}
	if patched["enabled"] != false {
		t.Fatalf("enabled = %v after disabling it", patched["enabled"])
	}

	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, convID, recallQuestion)
	text, done := answer(t, events)
	message, _ := done["message"].(map[string]any)
	if sources, _ := message["sources"].([]any); len(sources) != 0 {
		t.Fatalf("a disabled memory was retrieved: %v", sources)
	}
	if strings.Contains(text, "[S") {
		t.Fatalf("a disabled memory was cited: %q", text)
	}

	// Still listed, and the ?enabled= filter tells the two apart.
	if count, _ := listMemories(t, srv, alice, "")["count"].(float64); count != 1 {
		t.Fatal("the disabled memory disappeared from the list")
	}
	if count, _ := listMemories(t, srv, alice, "?enabled=true")["count"].(float64); count != 0 {
		t.Fatal("the disabled memory is still listed as enabled")
	}
	if count, _ := listMemories(t, srv, alice, "?enabled=false")["count"].(float64); count != 1 {
		t.Fatal("the disabled memory is not listed as disabled")
	}
	if count, _ := listMemories(t, srv, alice, "?type=preference")["count"].(float64); count != 1 {
		t.Fatal("the ?type= filter does not find the memory")
	}
	if count, _ := listMemories(t, srv, alice, "?type=goal")["count"].(float64); count != 0 {
		t.Fatal("the ?type= filter matched the wrong type")
	}
}

// Editing a memory's text has to move its vector, or the correction is
// retrievable by what it used to say and invisible to what it now says.
func TestEditingAMemoryChangesWhatItIsRetrievedBy(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")

	stored := remember(t, srv, alice)
	id, _ := stored["id"].(string)

	const corrected = "The user prefers revising late at night."
	resp, patched := doJSON(t, srv, http.MethodPatch, "/api/v1/memories/"+id, alice,
		map[string]any{"content": corrected})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH = %d: %v", resp.StatusCode, patched)
	}
	if patched["content"] != corrected {
		t.Fatalf("content = %v", patched["content"])
	}

	// The old wording no longer retrieves it.
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, convID, recallQuestion)
	_, done := answer(t, events)
	message, _ := done["message"].(map[string]any)
	if sources, _ := message["sources"].([]any); len(sources) != 0 {
		t.Fatalf("the edited memory is still retrievable by its old text: %v", sources)
	}

	// The new wording does.
	_, events = ask(t, srv, alice, convID, "When do I prefer revising late at night?")
	_, done = answer(t, events)
	message, _ = done["message"].(map[string]any)
	sources, _ := message["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("the edited memory is not retrievable by its new text: %v", sources)
	}
	if got, _ := sources[0].(map[string]any); got["id"] != id {
		t.Fatalf("retrieved %v, want the edited memory %v", got["id"], id)
	}
}

// "Forget everything about me" is irreversible, so an unconfirmed request must
// not be treated as one.
func TestClearingEveryMemoryNeedsConfirmation(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")
	remember(t, srv, alice)

	for _, body := range []any{map[string]any{}, map[string]any{"confirm": false}} {
		resp, out := doJSON(t, srv, http.MethodDelete, "/api/v1/memories", alice, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("DELETE /memories %v = %d, want 400: %v", body, resp.StatusCode, out)
		}
		if out["error"] != "validation_failed" {
			t.Fatalf("error = %v, want validation_failed", out["error"])
		}
	}
	if count, _ := listMemories(t, srv, alice, "")["count"].(float64); count != 1 {
		t.Fatal("an unconfirmed clear deleted the memory")
	}

	resp, cleared := doJSON(t, srv, http.MethodDelete, "/api/v1/memories", alice, map[string]any{"confirm": true})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirmed clear = %d: %v", resp.StatusCode, cleared)
	}
	if deleted, _ := cleared["deleted"].(float64); deleted != 1 {
		t.Fatalf("deleted = %v, want 1", cleared["deleted"])
	}
	if count, _ := listMemories(t, srv, alice, "")["count"].(float64); count != 0 {
		t.Fatal("a memory survived the confirmed clear")
	}

	// The assistant stops using what it no longer knows.
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, alice, convID, recallQuestion)
	_, done := answer(t, events)
	message, _ := done["message"].(map[string]any)
	if sources, _ := message["sources"].([]any); len(sources) != 0 {
		t.Fatalf("a cleared memory was retrieved: %v", sources)
	}
}

// Deleting a conversation must not delete what was learned in it, and deleting
// a user must take everything.
func TestMemoryDeletionCascades(t *testing.T) {
	srv := isolationServerWithProvider(t, rememberingProvider())
	alice := register(t, srv, "alice@example.com")

	stored := remember(t, srv, alice)
	source, _ := stored["source_conversation_id"].(string)

	resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/conversations/"+source, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete conversation = %d: %v", resp.StatusCode, body)
	}
	list := listMemories(t, srv, alice, "")
	items, _ := list["memories"].([]any)
	if len(items) != 1 {
		t.Fatalf("the memory went with its conversation: %v", list)
	}
	if survivor, _ := items[0].(map[string]any); survivor["source_conversation_id"] != nil {
		t.Fatalf("source_conversation_id = %v, want null once the conversation is gone", survivor["source_conversation_id"])
	}

	pool := mustPool(t)
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d memories survived the deleted user", n)
	}
}
