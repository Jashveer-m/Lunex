package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Phase 8 half of the property the whole ownership design exists for, over
// the real router, the real service and real SQL.
//
// These are skipped unless TEST_DATABASE_URL is set; see docs/testing.md.

// The window every test below asks for, chosen so the seeded events fall in it.
const (
	eventStart = "2026-09-17T09:00:00Z"
	eventEnd   = "2026-09-17T10:00:00Z"
	weekStart  = "2026-09-14"
	weekEnd    = "2026-09-21"
)

func weekOf(t *testing.T, srv *httptest.Server, token string) map[string]any {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet,
		"/api/v1/calendar?start="+weekStart+"&end="+weekEnd, token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /calendar: status %d: %v", resp.StatusCode, body)
	}
	return body
}

func TestCrossUserCalendarIsolation(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	eventID := create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Alice's appointment", "start_time": eventStart, "end_time": eventEnd,
	})

	// Alice's event exists and Bob does not own it. The answer must be 404 --
	// a 403 would confirm the id is real.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get event", http.MethodGet, "/api/v1/calendar/" + eventID, nil},
		{"patch event", http.MethodPatch, "/api/v1/calendar/" + eventID, map[string]any{"title": "hijacked"}},
		{"move event", http.MethodPatch, "/api/v1/calendar/" + eventID, map[string]any{"start_time": "2026-09-18T09:00:00Z"}},
		{"delete event", http.MethodDelete, "/api/v1/calendar/" + eventID, nil},
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

	// Bob's week never contains Alice's event, even when he searches for its
	// exact words -- the read is a range query scoped to the owner, not a
	// filter applied afterwards.
	if count, _ := weekOf(t, srv, bob)["count"].(float64); count != 0 {
		t.Fatalf("Bob sees %v events, want 0", count)
	}
	resp, found := doJSON(t, srv, http.MethodGet,
		"/api/v1/calendar?start="+weekStart+"&end="+weekEnd+"&q=Alice", bob, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /calendar?q=: %d %v", resp.StatusCode, found)
	}
	if count, _ := found["count"].(float64); count != 0 {
		t.Fatalf("Bob's search returned %v of Alice's events", count)
	}

	// And none of that touched Alice's event: every attempt was a no-op, not
	// just an unhelpful status code.
	resp, event := doJSON(t, srv, http.MethodGet, "/api/v1/calendar/"+eventID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Alice's event after Bob's attempts = %d %v", resp.StatusCode, event)
	}
	if event["title"] != "Alice's appointment" || event["start_time"] != "2026-09-17T09:00:00.000Z" {
		t.Fatalf("Alice's event after Bob's attempts = %v", event)
	}
}

// Linking an event to another user's task or goal would leak the existence of
// a foreign id through an otherwise-successful write.
func TestCrossUserEventLinksAreRejected(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")

	aliceTask := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Alice's task"})
	aliceGoal := create(t, srv, alice, "/api/v1/goals", map[string]any{"title": "Alice's goal", "type": "career"})
	bobEvent := create(t, srv, bob, "/api/v1/calendar", map[string]any{
		"title": "Bob's event", "start_time": eventStart, "end_time": eventEnd,
	})

	for name, body := range map[string]any{
		"another user's task": map[string]any{
			"title": "x", "start_time": eventStart, "end_time": eventEnd, "related_task_id": aliceTask,
		},
		"another user's goal": map[string]any{
			"title": "x", "start_time": eventStart, "end_time": eventEnd, "related_goal_id": aliceGoal,
		},
		"a task that does not exist": map[string]any{
			"title": "x", "start_time": eventStart, "end_time": eventEnd,
			"related_task_id": "11111111-1111-4111-8111-111111111111",
		},
	} {
		t.Run(name, func(t *testing.T) {
			resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/calendar", bob, body)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("linking to %s = %d, want 404: %v", name, resp.StatusCode, out)
			}
		})
	}
	resp, out := doJSON(t, srv, http.MethodPatch, "/api/v1/calendar/"+bobEvent, bob,
		map[string]any{"related_task_id": aliceTask})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("patching in another user's task = %d, want 404: %v", resp.StatusCode, out)
	}

	// Nothing was created behind those refusals, and Bob's own event is
	// untouched.
	if count, _ := weekOf(t, srv, bob)["count"].(float64); count != 1 {
		t.Fatalf("Bob has %v events, want only the one he made", count)
	}
	resp, event := doJSON(t, srv, http.MethodGet, "/api/v1/calendar/"+bobEvent, bob, nil)
	if resp.StatusCode != http.StatusOK || event["related_task_id"] != nil {
		t.Fatalf("Bob's event = %d %v, want no link", resp.StatusCode, event)
	}
}

// The range is the query: an unbounded read is not offered, and the absence of
// either end is a field error rather than "here is everything".
func TestCalendarListRequiresARange(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")
	create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Appointment", "start_time": eventStart, "end_time": eventEnd,
	})

	for name, query := range map[string]string{
		"no range":     "",
		"no end":       "?start=" + weekStart,
		"no start":     "?end=" + weekEnd,
		"backwards":    "?start=" + weekEnd + "&end=" + weekStart,
		"unreadable":   "?start=soon&end=" + weekEnd,
		"a decade out": "?start=2026-01-01&end=2050-01-01",
	} {
		t.Run(name, func(t *testing.T) {
			resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/calendar"+query, alice, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("GET /calendar%s = %d, want 400: %v", query, resp.StatusCode, body)
			}
			if body["error"] != "validation_failed" {
				t.Fatalf("error = %v, want validation_failed", body["error"])
			}
		})
	}

	// The control: the same read with a range answers, echoes the window back,
	// and finds the event.
	week := weekOf(t, srv, alice)
	if count, _ := week["count"].(float64); count != 1 {
		t.Fatalf("the owner's week = %v", week)
	}
	if week["start"] != "2026-09-14T00:00:00.000Z" || week["end"] != "2026-09-21T00:00:00.000Z" {
		t.Fatalf("the window was not echoed back: %v", week)
	}
}

// An event that began before the window and is still running is on that day's
// calendar. A query that only matched events *starting* inside the window
// would hide exactly the events most worth seeing.
func TestCalendarReturnsEventsOverlappingTheWindow(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Conference", "start_time": "2026-09-15T09:00:00Z", "end_time": "2026-09-18T17:00:00Z",
	})
	create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Reminder", "start_time": "2026-09-17T08:00:00Z", "end_time": "2026-09-17T08:00:00Z",
	})
	create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Next month", "start_time": "2026-10-17T09:00:00Z", "end_time": "2026-10-17T10:00:00Z",
	})

	// One day: the conference running through it, and the zero-length reminder
	// inside it.
	resp, day := doJSON(t, srv, http.MethodGet,
		"/api/v1/calendar?start=2026-09-17&end=2026-09-18", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /calendar: %d %v", resp.StatusCode, day)
	}
	var titles []string
	for _, raw := range day["events"].([]any) {
		e, _ := raw.(map[string]any)
		titles = append(titles, fmt.Sprint(e["title"]))
	}
	if fmt.Sprint(titles) != "[Conference Reminder]" {
		t.Fatalf("the day holds %v, want the overlapping conference and the reminder, in time order", titles)
	}

	// A day that ends before the conference starts does not include it.
	resp, quiet := doJSON(t, srv, http.MethodGet,
		"/api/v1/calendar?start=2026-09-14&end=2026-09-15", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /calendar: %d %v", resp.StatusCode, quiet)
	}
	if count, _ := quiet["count"].(float64); count != 0 {
		t.Fatalf("a day before everything returned %v events", count)
	}
}

// Deleting a user takes their calendar; deleting a task the event was for
// leaves the event and clears the link -- the hour is still booked even when
// the reason for it is gone.
func TestCalendarDeletionCascades(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	taskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Write the brief"})
	eventID := create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Writing time", "start_time": eventStart, "end_time": eventEnd,
		"related_task_id": taskID,
	})

	resp, _ := doJSON(t, srv, http.MethodDelete, "/api/v1/tasks/"+taskID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete task = %d", resp.StatusCode)
	}
	resp, event := doJSON(t, srv, http.MethodGet, "/api/v1/calendar/"+eventID, alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the event went with its task: %d %v", resp.StatusCode, event)
	}
	if event["related_task_id"] != nil {
		t.Fatalf("related_task_id = %v, want null once the task is gone", event["related_task_id"])
	}

	pool := mustPool(t)
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(`SELECT count(*) FROM calendar_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d events survived the deleted user", n)
	}
}

// An event gets a graph node like every other mirrored row, and the node
// follows the title and goes with the event.
func TestAnEventGetsAGraphNode(t *testing.T) {
	srv := isolationServer(t)
	alice := register(t, srv, "alice@example.com")

	eventID := create(t, srv, alice, "/api/v1/calendar", map[string]any{
		"title": "Dentist", "start_time": eventStart, "end_time": eventEnd,
	})

	node := func() map[string]any {
		t.Helper()
		resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=event", alice, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /knowledge-graph?type=event: %d %v", resp.StatusCode, body)
		}
		nodes, _ := body["nodes"].([]any)
		if len(nodes) != 1 {
			t.Fatalf("the event has %d nodes, want 1: %v", len(nodes), body)
		}
		n, _ := nodes[0].(map[string]any)
		return n
	}

	n := node()
	if n["ref_table"] != "calendar_events" || n["ref_id"] != eventID || n["label"] != "Dentist" {
		t.Fatalf("node = %v", n)
	}

	// Renaming the event renames the node: a node carrying a name the user has
	// stopped using is one the chat mention scan matches on the wrong thing.
	resp, _ := doJSON(t, srv, http.MethodPatch, "/api/v1/calendar/"+eventID, alice,
		map[string]any{"title": "Dentist checkup"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d", resp.StatusCode)
	}
	if label := node()["label"]; label != "Dentist checkup" {
		t.Fatalf("the node label is %v, want the new title", label)
	}

	// And a mirrored node cannot be deleted on its own -- the way to remove it
	// is to delete the event, which takes it through the trigger.
	nodeID, _ := node()["id"].(string)
	resp, body := doJSON(t, srv, http.MethodDelete, "/api/v1/knowledge-graph/nodes/"+nodeID, alice, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a mirrored node = %d, want 409: %v", resp.StatusCode, body)
	}
	resp, _ = doJSON(t, srv, http.MethodDelete, "/api/v1/calendar/"+eventID, alice, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete event = %d", resp.StatusCode)
	}
	resp, graph := doJSON(t, srv, http.MethodGet, "/api/v1/knowledge-graph?type=event", alice, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}
	if nodes, _ := graph["nodes"].([]any); len(nodes) != 0 {
		t.Fatalf("%d event nodes survived the deleted event", len(nodes))
	}
}
