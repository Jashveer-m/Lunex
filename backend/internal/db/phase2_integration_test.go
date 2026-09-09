package db_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/optional"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/users"
)

// These cover the Phase 2 SQL itself: the parts that no fake can stand in for —
// array round-trips, partial updates, the recursive cycle check and the
// cascades. Cross-user isolation over the whole stack lives in
// internal/api/isolation_test.go.

// makeUser creates the owner every Phase 2 row hangs off; the tables have a
// foreign key to users, so there is no shortcut around it.
func makeUser(t *testing.T, pool *sql.DB, email string) uuid.UUID {
	t.Helper()
	u, _, err := users.NewRepository(pool).Create(context.Background(), email, "hash", email, "UTC")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

func TestTaskRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	deadline := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Millisecond)
	estimate := 90
	created, err := repo.Create(ctx, owner, tasks.CreateInput{
		Title: "Write the brief", Description: "the whole thing", Priority: "high",
		Status: "pending", Category: "work", Tags: []string{"writing", "urgent"},
		Deadline: &deadline, EstimatedEffortMinutes: &estimate,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// text[] survives the driver round trip in order.
	if len(created.Tags) != 2 || created.Tags[0] != "writing" || created.Tags[1] != "urgent" {
		t.Fatalf("tags = %q, want [writing urgent]", created.Tags)
	}
	if created.Deadline == nil || !created.Deadline.Equal(deadline) {
		t.Fatalf("deadline = %v, want %v", created.Deadline, deadline)
	}
	if created.EstimatedEffortMinutes == nil || *created.EstimatedEffortMinutes != 90 {
		t.Fatalf("estimate = %v, want 90", created.EstimatedEffortMinutes)
	}

	got, err := repo.ByID(ctx, owner, created.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.Title != created.Title || len(got.Tags) != 2 {
		t.Fatalf("round trip lost data: %+v", got)
	}

	// A task with no tags reads back as an empty slice, not a NULL.
	bare, err := repo.Create(ctx, owner, tasks.CreateInput{Title: "bare", Priority: "low", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if bare.Tags == nil || len(bare.Tags) != 0 {
		t.Fatalf("tags = %v, want an empty slice", bare.Tags)
	}
	if bare.Description != nil || bare.Category != nil || bare.Deadline != nil {
		t.Fatalf("omitted optional columns are not NULL: %+v", bare)
	}

	if _, err := repo.ByID(ctx, owner, uuid.New()); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("ByID(unknown) = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, owner, uuid.New()); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("Delete(unknown) = %v, want ErrNotFound", err)
	}
}

// A partial update must leave every column it did not name alone, and an
// explicit null must actually clear a nullable one.
func TestTaskUpdateIsPartial(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	deadline := time.Now().Add(time.Hour)
	created, err := repo.Create(ctx, owner, tasks.CreateInput{
		Title: "t", Description: "keep me", Priority: "low", Status: "pending",
		Category: "work", Tags: []string{"a"}, Deadline: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := repo.Update(ctx, owner, created.ID, tasks.Patch{Status: strPtr("completed")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != "completed" {
		t.Fatalf("status = %q, want completed", updated.Status)
	}
	if updated.Description == nil || *updated.Description != "keep me" ||
		updated.Category == nil || len(updated.Tags) != 1 || updated.Deadline == nil {
		t.Fatalf("a column the patch did not name was overwritten: %+v", updated)
	}
	if !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated_at = %v, want the trigger to have moved it past %v", updated.UpdatedAt, created.UpdatedAt)
	}

	cleared, err := repo.Update(ctx, owner, created.ID, tasks.Patch{
		Description: optional.Null[string](),
		Deadline:    optional.Null[time.Time](),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cleared.Description != nil || cleared.Deadline != nil {
		t.Fatalf("an explicit null did not clear the column: %+v", cleared)
	}
	if cleared.Category == nil {
		t.Fatal("clearing description also cleared category")
	}

	// An empty patch is a read, not a write.
	before := cleared.UpdatedAt
	same, err := repo.Update(ctx, owner, created.ID, tasks.Patch{})
	if err != nil {
		t.Fatal(err)
	}
	if !same.UpdatedAt.Equal(before) {
		t.Fatal("an empty patch still touched the row")
	}
}

func TestTaskListFilterAndSort(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	other := makeUser(t, pool, "bob@example.com")

	mk := func(title, status, category string, tags []string, priority string) tasks.Task {
		t.Helper()
		task, err := repo.Create(ctx, owner, tasks.CreateInput{
			Title: title, Status: status, Category: category, Tags: tags, Priority: priority,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	mk("a", "pending", "work", []string{"urgent"}, "low")
	mk("b", "completed", "work", []string{"later"}, "high")
	mk("c", "pending", "home", []string{"urgent", "chore"}, "medium")
	if _, err := repo.Create(ctx, other, tasks.CreateInput{Title: "not mine", Status: "pending", Priority: "high"}); err != nil {
		t.Fatal(err)
	}

	list := func(f tasks.Filter) []tasks.Task {
		t.Helper()
		f, err := tasks.ValidateFilter(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := repo.List(ctx, owner, f)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The owner clause is not optional: another user's task never appears.
	if got := list(tasks.Filter{}); len(got) != 3 {
		t.Fatalf("unfiltered list returned %d tasks, want 3", len(got))
	}
	if got := list(tasks.Filter{Status: "pending"}); len(got) != 2 {
		t.Fatalf("status filter returned %d, want 2", len(got))
	}
	if got := list(tasks.Filter{Category: "home"}); len(got) != 1 {
		t.Fatalf("category filter returned %d, want 1", len(got))
	}
	if got := list(tasks.Filter{Tag: "urgent"}); len(got) != 2 {
		t.Fatalf("tag filter returned %d, want 2", len(got))
	}
	if got := list(tasks.Filter{Tag: "nonexistent"}); len(got) != 0 {
		t.Fatalf("unknown tag returned %d, want 0", len(got))
	}
	if got := list(tasks.Filter{Status: "pending", Tag: "urgent", Category: "work"}); len(got) != 1 {
		t.Fatalf("combined filters returned %d, want 1", len(got))
	}

	// priority sorts by rank, not alphabetically ("high" < "low" as text).
	byPriority := list(tasks.Filter{Sort: "-priority"})
	if len(byPriority) != 3 || byPriority[0].Priority != "high" || byPriority[2].Priority != "low" {
		t.Fatalf("-priority order = %v", priorities(byPriority))
	}

	byTitle := list(tasks.Filter{Sort: "title"})
	if byTitle[0].Title != "a" || byTitle[2].Title != "c" {
		t.Fatalf("title order = %v", titles(byTitle))
	}

	// Paging is stable: two pages of one cover the first two of a full page.
	full := list(tasks.Filter{Sort: "title"})
	first := list(tasks.Filter{Sort: "title", Limit: 1})
	second := list(tasks.Filter{Sort: "title", Limit: 1, Offset: 1})
	if len(first) != 1 || len(second) != 1 || first[0].ID != full[0].ID || second[0].ID != full[1].ID {
		t.Fatalf("paging is unstable: %v then %v against %v", titles(first), titles(second), titles(full))
	}
}

// Deadline sorting must keep undated rows out of the way.
func TestTaskDeadlineSortPutsNullsLast(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	soon := time.Now().Add(time.Hour)
	later := time.Now().Add(48 * time.Hour)
	if _, err := repo.Create(ctx, owner, tasks.CreateInput{Title: "undated", Priority: "low", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, owner, tasks.CreateInput{Title: "later", Priority: "low", Status: "pending", Deadline: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(ctx, owner, tasks.CreateInput{Title: "soon", Priority: "low", Status: "pending", Deadline: &soon}); err != nil {
		t.Fatal(err)
	}

	f, _ := tasks.ValidateFilter(tasks.Filter{Sort: "deadline"})
	got, err := repo.List(ctx, owner, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Title != "soon" || got[1].Title != "later" || got[2].Title != "undated" {
		t.Fatalf("deadline order = %v, want [soon later undated]", titles(got))
	}
}

func TestTaskDependencies(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	other := makeUser(t, pool, "bob@example.com")

	mk := func(owner uuid.UUID, title string) tasks.Task {
		t.Helper()
		task, err := repo.Create(ctx, owner, tasks.CreateInput{Title: title, Priority: "medium", Status: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	a, b, c := mk(owner, "a"), mk(owner, "b"), mk(owner, "c")
	foreign := mk(other, "not mine")

	if err := repo.AddDependency(ctx, owner, a.ID, b.ID); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	// Adding the same edge again is the state the caller asked for, not an error.
	if err := repo.AddDependency(ctx, owner, a.ID, b.ID); err != nil {
		t.Fatalf("re-adding a dependency: %v", err)
	}
	got, err := repo.ByID(ctx, owner, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != b.ID {
		t.Fatalf("depends_on = %v, want [%s]", got.DependsOn, b.ID)
	}

	t.Run("a direct cycle is refused", func(t *testing.T) {
		if err := repo.AddDependency(ctx, owner, b.ID, a.ID); !errors.Is(err, tasks.ErrDependencyCycle) {
			t.Fatalf("err = %v, want ErrDependencyCycle", err)
		}
	})

	t.Run("a transitive cycle is refused", func(t *testing.T) {
		if err := repo.AddDependency(ctx, owner, b.ID, c.ID); err != nil {
			t.Fatal(err)
		}
		// a -> b -> c, so c -> a would close the loop.
		if err := repo.AddDependency(ctx, owner, c.ID, a.ID); !errors.Is(err, tasks.ErrDependencyCycle) {
			t.Fatalf("err = %v, want ErrDependencyCycle", err)
		}
	})

	t.Run("either endpoint belonging to somebody else is not found", func(t *testing.T) {
		if err := repo.AddDependency(ctx, owner, a.ID, foreign.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("depending on a foreign task = %v, want ErrNotFound", err)
		}
		if err := repo.AddDependency(ctx, other, foreign.ID, a.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("a foreign task depending on ours = %v, want ErrNotFound", err)
		}
		// And the caller must not be able to reach it by claiming the wrong owner.
		if err := repo.AddDependency(ctx, other, a.ID, b.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("edge between another user's tasks = %v, want ErrNotFound", err)
		}
	})

	t.Run("deleting a task removes its edges", func(t *testing.T) {
		if err := repo.Delete(ctx, owner, b.ID); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := pool.QueryRow(
			`SELECT count(*) FROM task_dependencies WHERE task_id = $1 OR depends_on_task_id = $1`, b.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d dependency rows survived the task deletion", n)
		}
	})
}

// The self-dependency CHECK is the database's own backstop for the rule the
// service enforces first.
func TestTaskSelfDependencyIsRejectedByTheDatabase(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	owner := makeUser(t, pool, "ada@example.com")
	task, err := tasks.NewRepository(pool).Create(ctx, owner, tasks.CreateInput{Title: "t", Priority: "low", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(
		`INSERT INTO task_dependencies (task_id, depends_on_task_id) VALUES ($1, $1)`, task.ID); err == nil {
		t.Fatal("the database accepted a task depending on itself")
	}
}

// A subtask goes with its parent, which is what ON DELETE CASCADE on
// parent_task_id is for.
func TestSubtaskCascadesWithItsParent(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := tasks.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")

	parent, err := repo.Create(ctx, owner, tasks.CreateInput{Title: "parent", Priority: "low", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := repo.Create(ctx, owner, tasks.CreateInput{
		Title: "child", Priority: "low", Status: "pending", ParentTaskID: &parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentTaskID == nil || *child.ParentTaskID != parent.ID {
		t.Fatalf("parent_task_id = %v, want %s", child.ParentTaskID, parent.ID)
	}

	if err := repo.Delete(ctx, owner, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ByID(ctx, owner, child.ID); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("the subtask survived its parent: %v", err)
	}
}

func TestGoalRepositoryAndMilestones(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := goals.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	other := makeUser(t, pool, "bob@example.com")

	goal, err := repo.Create(ctx, owner, goals.CreateInput{
		Title: "Learn Go", Description: "properly", Type: "education", Status: "active",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if goal.Milestones == nil {
		t.Fatal("a new goal has nil milestones; want an empty slice")
	}

	// Milestones come back ordered by target date, undated last.
	soon := time.Now().Add(24 * time.Hour)
	later := time.Now().Add(96 * time.Hour)
	if _, err := repo.AddMilestone(ctx, owner, goal.ID, goals.MilestoneInput{Title: "undated"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AddMilestone(ctx, owner, goal.ID, goals.MilestoneInput{Title: "later", TargetDate: &later}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.AddMilestone(ctx, owner, goal.ID, goals.MilestoneInput{Title: "soon", TargetDate: &soon})
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := repo.ByID(ctx, owner, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Milestones) != 3 {
		t.Fatalf("%d milestones, want 3", len(loaded.Milestones))
	}
	if loaded.Milestones[0].Title != "soon" || loaded.Milestones[2].Title != "undated" {
		t.Fatalf("milestone order = %v", milestoneTitles(loaded.Milestones))
	}

	t.Run("the list endpoint batch-loads milestones", func(t *testing.T) {
		f, _ := goals.ValidateFilter(goals.Filter{})
		list, err := repo.List(ctx, owner, f)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || len(list[0].Milestones) != 3 {
			t.Fatalf("list = %d goals with %d milestones", len(list), len(list[0].Milestones))
		}
	})

	t.Run("milestones are reachable only through their owner's goal", func(t *testing.T) {
		if _, err := repo.AddMilestone(ctx, other, goal.ID, goals.MilestoneInput{Title: "hijack"}); !errors.Is(err, goals.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if _, err := repo.UpdateMilestone(ctx, other, goal.ID, first.ID,
			goals.MilestonePatch{Completed: boolPtr(true)}); !errors.Is(err, goals.ErrMilestoneNotFound) {
			t.Fatalf("err = %v, want ErrMilestoneNotFound", err)
		}
		if _, err := repo.CountMilestones(ctx, other, goal.ID); !errors.Is(err, goals.ErrNotFound) {
			t.Fatalf("CountMilestones as the wrong user = %v, want ErrNotFound", err)
		}
	})

	t.Run("a milestone patch is partial", func(t *testing.T) {
		updated, err := repo.UpdateMilestone(ctx, owner, goal.ID, first.ID, goals.MilestonePatch{Completed: boolPtr(true)})
		if err != nil {
			t.Fatal(err)
		}
		if !updated.Completed || updated.Title != "soon" || updated.TargetDate == nil {
			t.Fatalf("the patch overwrote a column it did not name: %+v", updated)
		}
	})

	t.Run("a goal patch is partial and clears on null", func(t *testing.T) {
		updated, err := repo.Update(ctx, owner, goal.ID, goals.Patch{Status: strPtr("completed")})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status != "completed" || updated.Description == nil || updated.Type != "education" {
			t.Fatalf("goal patch = %+v", updated)
		}
		cleared, err := repo.Update(ctx, owner, goal.ID, goals.Patch{Description: optional.Null[string]()})
		if err != nil {
			t.Fatal(err)
		}
		if cleared.Description != nil {
			t.Fatal("an explicit null did not clear the description")
		}
	})

	t.Run("deleting a goal takes its milestones", func(t *testing.T) {
		if err := repo.Delete(ctx, owner, goal.ID); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := pool.QueryRow(`SELECT count(*) FROM goal_milestones WHERE goal_id = $1`, goal.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d milestones survived the goal deletion", n)
		}
	})
}

func TestNoteRepositoryRoundTrip(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	repo := notes.NewRepository(pool)
	owner := makeUser(t, pool, "ada@example.com")
	other := makeUser(t, pool, "bob@example.com")

	created, err := repo.Create(ctx, owner, notes.CreateInput{
		Title: "Ideas", Content: "one\ntwo", Tags: []string{"draft", "later"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(created.Tags) != 2 || created.Content != "one\ntwo" {
		t.Fatalf("round trip lost data: %+v", created)
	}

	bare, err := repo.Create(ctx, owner, notes.CreateInput{Title: "bare", Tags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	// content and tags are NOT NULL with defaults, so an empty note is legal.
	if bare.Content != "" || bare.Tags == nil || len(bare.Tags) != 0 {
		t.Fatalf("empty note = %+v", bare)
	}

	if _, err := repo.Create(ctx, other, notes.CreateInput{Title: "not mine", Tags: []string{"draft"}}); err != nil {
		t.Fatal(err)
	}

	f, _ := notes.ValidateFilter(notes.Filter{Tag: "draft"})
	list, err := repo.List(ctx, owner, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("tag filter returned %d notes, want only the owner's", len(list))
	}

	t.Run("a patch is partial", func(t *testing.T) {
		updated, err := repo.Update(ctx, owner, created.ID, notes.Patch{Title: strPtr("Ideas v2")})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Title != "Ideas v2" || updated.Content != "one\ntwo" || len(updated.Tags) != 2 {
			t.Fatalf("the patch overwrote a column it did not name: %+v", updated)
		}
		if !updated.UpdatedAt.After(created.UpdatedAt) {
			t.Fatal("the updated_at trigger did not fire on notes")
		}
	})

	t.Run("tags can be emptied", func(t *testing.T) {
		empty := []string{}
		updated, err := repo.Update(ctx, owner, created.ID, notes.Patch{Tags: &empty})
		if err != nil {
			t.Fatal(err)
		}
		if len(updated.Tags) != 0 {
			t.Fatalf("tags = %q, want them emptied", updated.Tags)
		}
	})

	if err := repo.Delete(ctx, other, created.ID); !errors.Is(err, notes.ErrNotFound) {
		t.Fatalf("deleting another user's note = %v, want ErrNotFound", err)
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func titles(ts []tasks.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Title)
	}
	return out
}

func priorities(ts []tasks.Task) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Priority)
	}
	return out
}

func milestoneTitles(ms []goals.Milestone) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Title)
	}
	return out
}
