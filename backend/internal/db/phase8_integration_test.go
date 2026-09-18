package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/calendar"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/tasks"
)

// These cover the Phase 8 SQL: the overlap query that is the whole of the
// calendar read, the interval CHECK, the two cascades that differ (a user takes
// their events, a task leaves them behind), and the node the trigger removes.
// Cross-user isolation over the whole stack lives in
// internal/api/calendar_isolation_test.go.

// The week these tests work in: Monday 14 to Monday 21 September 2026.
var (
	monday   = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	thursday = monday.AddDate(0, 0, 3)
)

func at(day time.Time, hour int) time.Time { return day.Add(time.Duration(hour) * time.Hour) }

func seedEvent(t *testing.T, repo *calendar.Repository, owner uuid.UUID, title string, start, end time.Time) calendar.Event {
	t.Helper()
	e, err := repo.Create(context.Background(), owner, calendar.CreateInput{
		Title: title, StartTime: start, EndTime: end,
	})
	if err != nil {
		t.Fatalf("create %s: %v", title, err)
	}
	return e
}

func TestCalendarRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := calendar.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	taskRepo := tasks.NewRepository(pool)
	task, err := taskRepo.Create(ctx, owner, tasks.CreateInput{Title: "Write the brief", Priority: "high", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	goal, err := goals.NewRepository(pool).Create(ctx, owner, goals.CreateInput{Title: "Ship it", Type: "career", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}

	rule := "FREQ=WEEKLY;BYDAY=MO"
	created, err := repo.Create(ctx, owner, calendar.CreateInput{
		Title: "Writing time", Description: "the whole thing", Location: "the shed",
		StartTime: at(thursday, 9), EndTime: at(thursday, 11),
		RecurrenceRule: rule, RelatedTaskID: &task.ID, RelatedGoalID: &goal.ID,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	switch {
	case !created.StartTime.Equal(at(thursday, 9)) || !created.EndTime.Equal(at(thursday, 11)):
		t.Fatalf("interval = %v to %v", created.StartTime, created.EndTime)
	case created.AllDay:
		t.Fatal("all_day defaulted to true")
	case created.RecurrenceRule == nil || *created.RecurrenceRule != rule:
		t.Fatalf("recurrence_rule = %v, want it stored as written", created.RecurrenceRule)
	case created.RelatedTaskID == nil || *created.RelatedTaskID != task.ID:
		t.Fatalf("related_task_id = %v", created.RelatedTaskID)
	case created.RelatedGoalID == nil || *created.RelatedGoalID != goal.ID:
		t.Fatalf("related_goal_id = %v", created.RelatedGoalID)
	}

	// A partial update touches only the columns it names -- and clearing a
	// nullable one is different from leaving it alone.
	moved, err := repo.Update(ctx, owner, created.ID, calendar.Patch{
		StartTime: ptr(at(thursday, 14)), EndTime: ptr(at(thursday, 15)),
		Location: optional.Null[string](),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	switch {
	case !moved.StartTime.Equal(at(thursday, 14)):
		t.Fatalf("start = %v", moved.StartTime)
	case moved.Location != nil:
		t.Fatalf("location = %v, want it cleared", *moved.Location)
	case moved.Title != "Writing time" || moved.Description == nil:
		t.Fatalf("an unmentioned column changed: %+v", moved)
	case !moved.UpdatedAt.After(moved.CreatedAt):
		t.Fatal("the set_updated_at trigger did not fire")
	}

	// The ownership probes the service links through: owner-scoped, so
	// another user's task is indistinguishable from one that does not exist.
	stranger := makeUser(t, pool, "bob@example.com")
	for name, tc := range map[string]struct {
		user uuid.UUID
		id   uuid.UUID
		want bool
	}{
		"the owner's task":       {owner, task.ID, true},
		"somebody else's task":   {stranger, task.ID, false},
		"a task that is not one": {owner, uuid.New(), false},
	} {
		got, err := repo.TaskExists(ctx, tc.user, tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("%s: TaskExists = %v, %v; want %v", name, got, err, tc.want)
		}
	}
	if got, err := repo.GoalExists(ctx, owner, goal.ID); err != nil || !got {
		t.Fatalf("GoalExists = %v, %v", got, err)
	}

	if err := repo.Delete(ctx, owner, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ByID(ctx, owner, created.ID); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatalf("ByID after delete = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, owner, created.ID); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatalf("a second delete = %v, want ErrNotFound", err)
	}
}

// The range query, which is the whole of the calendar read: an event is in the
// window when it overlaps it, and the endpoints are half-open.
func TestCalendarRangeQueryReturnsOverlaps(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := calendar.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	// Thursday is the window: [Thu 00:00, Fri 00:00).
	window := calendar.Filter{
		Start: thursday, End: thursday.AddDate(0, 0, 1),
		Sort: calendar.DefaultSort, Limit: calendar.DefaultLimit,
	}

	seedEvent(t, repo, owner, "runs through it", at(monday, 9), at(thursday.AddDate(0, 0, 1), 17))
	seedEvent(t, repo, owner, "inside it", at(thursday, 9), at(thursday, 10))
	seedEvent(t, repo, owner, "starts in it", at(thursday, 23), at(thursday.AddDate(0, 0, 2), 9))
	// A zero-length event: a reminder at a moment, which the CHECK allows and
	// `end > start` alone would miss.
	seedEvent(t, repo, owner, "a moment in it", at(thursday, 8), at(thursday, 8))
	// The half-open endpoints: an event ending exactly at the window's start,
	// and one starting exactly at its end, are both outside.
	seedEvent(t, repo, owner, "ends as it begins", at(thursday.AddDate(0, 0, -1), 20), thursday)
	seedEvent(t, repo, owner, "starts as it ends", thursday.AddDate(0, 0, 1), at(thursday.AddDate(0, 0, 1), 1))
	seedEvent(t, repo, owner, "next month", at(thursday.AddDate(0, 1, 0), 9), at(thursday.AddDate(0, 1, 0), 10))

	found, err := repo.List(ctx, owner, window)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range found {
		titles = append(titles, e.Title)
	}
	want := []string{"runs through it", "a moment in it", "inside it", "starts in it"}
	if len(titles) != len(want) {
		t.Fatalf("the day holds %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("the day holds %v, want %v (chronological)", titles, want)
		}
	}

	// The text filter searches title, description and location, inside the
	// window and never outside it.
	byText := window
	byText.Query = "inside"
	if found, err = repo.List(ctx, owner, byText); err != nil || len(found) != 1 {
		t.Fatalf("q=inside found %d: %v", len(found), err)
	}
	// `%` is a character, not a wildcard.
	byText.Query = "%"
	if found, err = repo.List(ctx, owner, byText); err != nil || len(found) != 0 {
		t.Fatalf("q=%% found %d, want 0: %v", len(found), err)
	}

	// Paging is applied to the window, in order.
	paged := window
	paged.Limit, paged.Offset = 2, 1
	if found, err = repo.List(ctx, owner, paged); err != nil || len(found) != 2 || found[0].Title != "a moment in it" {
		t.Fatalf("page = %v: %v", found, err)
	}
}

func TestCalendarConstraints(t *testing.T) {
	pool := testDB(t)
	owner := makeUser(t, pool, "ada@example.com")

	t.Run("an event that ends before it starts is rejected", func(t *testing.T) {
		if _, err := pool.Exec(`
			INSERT INTO calendar_events (user_id, title, start_time, end_time)
			VALUES ($1, 'backwards', $2, $3)`, owner, at(thursday, 10), at(thursday, 9)); err == nil {
			t.Fatal("an inverted interval was stored")
		}
	})

	t.Run("a zero-length event is allowed", func(t *testing.T) {
		if _, err := pool.Exec(`
			INSERT INTO calendar_events (user_id, title, start_time, end_time)
			VALUES ($1, 'a moment', $2, $2)`, owner, at(thursday, 9)); err != nil {
			t.Fatalf("a reminder at a moment was refused: %v", err)
		}
	})

	t.Run("an event must belong to a real user", func(t *testing.T) {
		if _, err := pool.Exec(`
			INSERT INTO calendar_events (user_id, title, start_time, end_time)
			VALUES ($1, 'orphan', $2, $3)`, uuid.New(), at(thursday, 9), at(thursday, 10)); err == nil {
			t.Fatal("an event was stored for a user that does not exist")
		}
	})

	t.Run("an event node is storable and an unknown type still is not", func(t *testing.T) {
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label) VALUES ($1, 'event', 'Dentist')`, owner); err != nil {
			t.Fatalf("migration 000008 did not widen the node type allow-list: %v", err)
		}
		if _, err := pool.Exec(
			`INSERT INTO knowledge_nodes (user_id, type, label) VALUES ($1, 'invoice', 'coffee')`, owner); err == nil {
			t.Fatal("a node type outside the allow-list was stored")
		}
	})
}

// The two cascades, which differ on purpose: a deleted user takes their
// calendar, and a deleted task leaves the hour that was booked for it.
func TestCalendarCascades(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := calendar.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	task, err := tasks.NewRepository(pool).Create(ctx, owner, tasks.CreateInput{
		Title: "Write the brief", Priority: "medium", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := repo.Create(ctx, owner, calendar.CreateInput{
		Title: "Writing time", StartTime: at(thursday, 9), EndTime: at(thursday, 11), RelatedTaskID: &task.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(`DELETE FROM tasks WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}
	survivor, err := repo.ByID(ctx, owner, event.ID)
	if err != nil {
		t.Fatalf("the event went with its task: %v", err)
	}
	if survivor.RelatedTaskID != nil {
		t.Fatalf("related_task_id = %v, want NULL once the task is gone", survivor.RelatedTaskID)
	}

	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, pool, "calendar_events"); n != 0 {
		t.Fatalf("%d events survived the deleted user", n)
	}
}

// The node sync round trip, against the real graph: the ref_table string this
// module writes is one migration 000008 allows, and the trigger takes the node
// when the event goes -- including on a path no service is on.
func TestAnEventNodeFollowsItsRow(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	graphSvc := graph.NewService(graph.Deps{Store: graph.NewRepository(pool)})
	svc := calendar.NewService(calendar.NewRepository(pool), calendar.WithNodeSync(graphSvc))
	owner := makeUser(t, pool, "ada@example.com")

	event, err := svc.Create(ctx, owner, calendar.CreateInput{
		Title: "Dentist", StartTime: at(thursday, 9), EndTime: at(thursday, 10),
	})
	if err != nil {
		t.Fatal(err)
	}

	node := func() (graph.Node, bool) {
		t.Helper()
		g, err := graphSvc.Graph(ctx, owner, graph.Filter{Type: graph.NodeEvent, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Nodes) == 0 {
			return graph.Node{}, false
		}
		return g.Nodes[0], true
	}

	n, ok := node()
	if !ok {
		t.Fatal("creating an event created no node")
	}
	switch {
	case n.RefTable == nil || *n.RefTable != "calendar_events":
		t.Fatalf("ref_table = %v", n.RefTable)
	case n.RefID == nil || *n.RefID != event.ID:
		t.Fatalf("ref_id = %v, want the event", n.RefID)
	case n.Label != "Dentist" || n.Type != graph.NodeEvent:
		t.Fatalf("node = %+v", n)
	}

	// Syncing again is an upsert, not a second node.
	if _, err := svc.Update(ctx, owner, event.ID, calendar.UpdateInput{Title: optional.Of("Dentist checkup")}); err != nil {
		t.Fatal(err)
	}
	if n, _ = node(); n.Label != "Dentist checkup" {
		t.Fatalf("label = %q, want the new title", n.Label)
	}
	if got := countRows(t, pool, "knowledge_nodes"); got != 1 {
		t.Fatalf("%d nodes after a rename, want 1", got)
	}

	// The trigger fires on a delete no service made.
	if _, err := pool.Exec(`DELETE FROM calendar_events WHERE id = $1`, event.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := node(); ok {
		t.Fatal("the node survived its event")
	}
}
