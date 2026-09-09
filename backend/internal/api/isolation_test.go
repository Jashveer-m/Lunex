package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
	// Tasks, goals and notes all cascade from users, so one truncate is enough.
	if _, err := pool.Exec(`TRUNCATE users CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return newServer(t, pool)
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
