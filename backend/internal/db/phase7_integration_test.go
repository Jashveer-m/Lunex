package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// These cover the Phase 7 SQL: the actions table and its constraints, the
// conditional UPDATE that is the approval gate, the insert that shares the chat
// turn's transaction, the cascades, and the text filter the search tools use.
// The central test is the brief's: a write tool never executes without an
// approved action row -- here against the real registry, the real task
// service and the real table.

// stack is the real registry over the real services, with the real actions
// repository as its ledger -- the production wiring minus HTTP.
type stack struct {
	pool    *sql.DB
	actions *actions.Repository
	reg     *tools.Registry
	tasks   *tasks.Repository
}

func newStack(t *testing.T) stack {
	t.Helper()
	pool := testDB(t)
	s := stack{pool: pool, actions: actions.NewRepository(pool), tasks: tasks.NewRepository(pool)}
	reg, err := tools.NewRegistry(s.actions, tools.Standard(tools.Services{
		Tasks: tasks.NewService(s.tasks),
		Goals: goals.NewService(goals.NewRepository(pool)),
		Notes: notes.NewService(notes.NewRepository(pool)),
	})...)
	if err != nil {
		t.Fatal(err)
	}
	s.reg = reg
	return s
}

func (s stack) prepare(t *testing.T, owner uuid.UUID, tool string, args tools.Args) tools.Call {
	t.Helper()
	call, err := s.reg.Prepare(context.Background(), owner, tool, args)
	if err != nil {
		t.Fatalf("prepare %s: %v", tool, err)
	}
	return call
}

func (s stack) record(t *testing.T, owner uuid.UUID, conv *uuid.UUID, n actions.NewAction) actions.Action {
	t.Helper()
	out, err := s.actions.Record(context.Background(), owner, conv, n)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	return out[0]
}

func (s stack) countTasks(t *testing.T) int {
	t.Helper()
	return countRows(t, s.pool, "tasks")
}

// The brief's test. Every way a write could be reached without the proposed ->
// approved transition of the row naming it, run against the real table, and
// the tasks table counted after each.
func TestAWriteToolNeverExecutesWithoutAnApprovedActionRow(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	bob := makeUser(t, s.pool, "bob@example.com")
	call := s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "Renew the passport"})

	// Asked to run it as a read.
	if _, err := s.reg.RunRead(ctx, alice, call); !errors.Is(err, tools.ErrApprovalRequired) {
		t.Fatalf("RunRead(create_task) = %v", err)
	}

	// Rows in every state but proposed, and rows that are not the caller's.
	proposed := s.record(t, alice, nil, actions.Proposal(call))
	bobsCall := s.prepare(t, bob, tools.CreateTask, tools.Args{"title": "Bob's task"})
	bobs := s.record(t, bob, nil, actions.Proposal(bobsCall))
	rejected := s.record(t, alice, nil, actions.Proposal(call))
	if _, err := s.actions.Reject(ctx, alice, rejected.ID); err != nil {
		t.Fatal(err)
	}
	// A row somebody set to approved by hand is not an approval: the gate is
	// the transition out of proposed, and this row cannot make it.
	handApproved := s.record(t, alice, nil, actions.Proposal(call))
	if _, err := s.pool.Exec(`UPDATE actions SET status = 'approved' WHERE id = $1`, handApproved.ID); err != nil {
		t.Fatal(err)
	}
	read := s.record(t, alice, nil, actions.Ran(s.prepare(t, alice, tools.SearchTasks, tools.Args{}),
		tools.Result{Output: map[string]any{"count": 0}}))

	for name, tc := range map[string]struct {
		user, id uuid.UUID
		want     error
	}{
		"no such row":             {alice, uuid.New(), actions.ErrNotFound},
		"another user's proposal": {alice, bobs.ID, actions.ErrNotFound},
		"alice's, asked by bob":   {bob, proposed.ID, actions.ErrNotFound},
		"rejected":                {alice, rejected.ID, actions.ErrNotPending},
		"already approved":        {alice, handApproved.ID, actions.ErrNotPending},
		"a read":                  {alice, read.ID, actions.ErrNotPending},
	} {
		if _, err := s.reg.RunApproved(ctx, tc.user, tc.id); !errors.Is(err, tc.want) {
			t.Fatalf("%s: RunApproved = %v, want %v", name, err, tc.want)
		}
	}
	if n := s.countTasks(t); n != 0 {
		t.Fatalf("%d tasks exist and no action was approved", n)
	}

	// The approvable one runs -- once, as proposed -- and only then is there a
	// task.
	exec, err := s.reg.RunApproved(ctx, alice, proposed.ID)
	if err != nil || exec.Err != nil {
		t.Fatalf("RunApproved = %v / %v", err, exec.Err)
	}
	if n := s.countTasks(t); n != 1 {
		t.Fatalf("approving one action made %d tasks", n)
	}
	if _, err := s.reg.RunApproved(ctx, alice, proposed.ID); !errors.Is(err, actions.ErrNotPending) {
		t.Fatalf("a second RunApproved = %v, want ErrNotPending", err)
	}
	if n := s.countTasks(t); n != 1 {
		t.Fatalf("approving it twice made %d tasks", n)
	}
	// Bob's proposal is untouched by all of it.
	if got, _ := s.actions.ByID(ctx, bob, bobs.ID); got.Status != actions.StatusProposed {
		t.Fatalf("bob's action is %s", got.Status)
	}
}

// The gate under real concurrency: sixteen simultaneous approvals of one row,
// exactly one gets it.
func TestApproveIsSingleUseUnderConcurrency(t *testing.T) {
	s := newStack(t)
	alice := makeUser(t, s.pool, "alice@example.com")
	a := s.record(t, alice, nil, actions.Proposal(s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "Once"})))

	var wg sync.WaitGroup
	var mu sync.Mutex
	won, refused := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.reg.RunApproved(context.Background(), alice, a.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, actions.ErrNotPending):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || refused != 15 {
		t.Fatalf("%d won and %d were refused, want 1 and 15", won, refused)
	}
	if n := s.countTasks(t); n != 1 {
		t.Fatalf("sixteen concurrent approvals made %d tasks", n)
	}
}

func TestActionRoundTripAndFilters(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	conv := seedConversation(t, chat.NewRepository(s.pool), alice, "Planning")

	proposal := s.record(t, alice, &conv.ID, actions.Proposal(s.prepare(t, alice, tools.CreateTask,
		tools.Args{"title": "Buy milk", "deadline": "2026-09-30"})))
	read := s.record(t, alice, &conv.ID, actions.Ran(s.prepare(t, alice, tools.SearchTasks, tools.Args{"query": "milk"}),
		tools.Result{Output: map[string]any{"count": 0, "more": false, "tasks": []any{}}}))

	got, err := s.actions.ByID(ctx, alice, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	var in map[string]any
	if err := json.Unmarshal(got.Input, &in); err != nil || in["title"] != "Buy milk" || in["deadline"] != "2026-09-30T00:00:00Z" {
		t.Fatalf("input = %s (%v)", got.Input, err)
	}
	switch {
	case got.Status != actions.StatusProposed || got.Permission != tools.Write || got.Result != nil || got.ErrorMessage != nil:
		t.Fatalf("proposal = %+v", got)
	case got.ConversationID == nil || *got.ConversationID != conv.ID:
		t.Fatalf("conversation = %v", got.ConversationID)
	}
	if r, _ := s.actions.ByID(ctx, alice, read.ID); r.Status != actions.StatusExecuted || len(r.Result) == 0 {
		t.Fatalf("read = %+v", r)
	}

	for name, tc := range map[string]struct {
		f    actions.Filter
		want int
	}{
		"all":          {actions.Filter{Limit: 50}, 2},
		"proposed":     {actions.Filter{Status: actions.StatusProposed, Limit: 50}, 1},
		"reads":        {actions.Filter{Permission: "read", Limit: 50}, 1},
		"conversation": {actions.Filter{ConversationID: &conv.ID, Limit: 50}, 2},
		"other conv":   {actions.Filter{ConversationID: ptr(uuid.New()), Limit: 50}, 0},
		"paged":        {actions.Filter{Limit: 1}, 1},
	} {
		list, err := s.actions.List(ctx, alice, tc.f)
		if err != nil || len(list) != tc.want {
			t.Fatalf("%s: %d actions (%v), want %d", name, len(list), err, tc.want)
		}
	}
	// Newest first.
	list, _ := s.actions.List(ctx, alice, actions.Filter{Limit: 50})
	if list[0].ID != read.ID {
		t.Fatal("the list is not newest first")
	}
}

func ptr[T any](v T) *T { return &v }

// Every CHECK constraint, by writing straight past the application.
func TestActionConstraints(t *testing.T) {
	s := newStack(t)
	alice := makeUser(t, s.pool, "alice@example.com")
	for name, stmt := range map[string]string{
		"an unknown status":           `INSERT INTO actions (user_id, tool_name, input, permission_level, status) VALUES ($1, 'create_task', '{}', 'write', 'done')`,
		"an unknown permission":       `INSERT INTO actions (user_id, tool_name, input, permission_level) VALUES ($1, 'create_task', '{}', 'admin')`,
		"a proposed read":             `INSERT INTO actions (user_id, tool_name, input, permission_level, status) VALUES ($1, 'search_tasks', '{}', 'read', 'proposed')`,
		"an approved read":            `INSERT INTO actions (user_id, tool_name, input, permission_level, status) VALUES ($1, 'search_tasks', '{}', 'read', 'approved')`,
		"input that is not an object": `INSERT INTO actions (user_id, tool_name, input, permission_level) VALUES ($1, 'create_task', '[]', 'write')`,
		"executed with no result":     `INSERT INTO actions (user_id, tool_name, input, permission_level, status) VALUES ($1, 'search_tasks', '{}', 'read', 'executed')`,
		"a result on a proposal":      `INSERT INTO actions (user_id, tool_name, input, permission_level, result) VALUES ($1, 'create_task', '{}', 'write', '{}')`,
		"failed with no reason":       `INSERT INTO actions (user_id, tool_name, input, permission_level, status) VALUES ($1, 'search_tasks', '{}', 'read', 'failed')`,
		"a reason on a proposal":      `INSERT INTO actions (user_id, tool_name, input, permission_level, error_message) VALUES ($1, 'create_task', '{}', 'write', 'x')`,
	} {
		if _, err := s.pool.Exec(stmt, alice); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// And the default is proposed.
	var status string
	if err := s.pool.QueryRow(`INSERT INTO actions (user_id, tool_name, input, permission_level)
		VALUES ($1, 'create_task', '{"title":"x"}', 'write') RETURNING status`, alice).Scan(&status); err != nil || status != "proposed" {
		t.Fatalf("default status = %q (%v)", status, err)
	}
}

// Insert refuses a state the machine does not allow before any SQL runs, so a
// batch with one bad action writes none of them.
func TestInsertRefusesAWriteThatIsNotProposed(t *testing.T) {
	s := newStack(t)
	alice := makeUser(t, s.pool, "alice@example.com")
	call := s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "x"})
	_, err := s.actions.Record(context.Background(), alice, nil,
		actions.Proposal(call),
		actions.NewAction{Call: call, Status: actions.StatusExecuted, Result: map[string]any{}})
	if !errors.Is(err, actions.ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
	if n := countRows(t, s.pool, "actions"); n != 0 {
		t.Fatalf("%d actions were written from a refused batch", n)
	}
}

// Finish records an outcome once, and only for an approved action.
func TestFinishOnlyMovesAnApprovedAction(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	a := s.record(t, alice, nil, actions.Proposal(s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "x"})))

	if _, err := s.actions.Finish(ctx, alice, a.ID, actions.Outcome{Result: map[string]any{}}); !errors.Is(err, actions.ErrNotPending) {
		t.Fatalf("finishing a proposal = %v, want ErrNotPending", err)
	}
	if _, err := s.actions.Approve(ctx, alice, a.ID); err != nil {
		t.Fatal(err)
	}
	failed, err := s.actions.Finish(ctx, alice, a.ID, actions.Outcome{Failed: "It could not be done."})
	if err != nil || failed.Status != actions.StatusFailed || *failed.ErrorMessage != "It could not be done." {
		t.Fatalf("Finish = %+v, %v", failed, err)
	}
	if !failed.UpdatedAt.After(a.UpdatedAt) {
		t.Fatal("the set_updated_at trigger did not fire")
	}
	if _, err := s.actions.Finish(ctx, alice, a.ID, actions.Outcome{Result: map[string]any{}}); !errors.Is(err, actions.ErrNotPending) {
		t.Fatalf("finishing twice = %v, want ErrNotPending", err)
	}
}

// A turn's actions commit with its messages or not at all.
func TestTurnActionsCommitWithTheTurn(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	repo := chat.NewRepository(s.pool)
	conv := seedConversation(t, repo, alice, chat.DefaultTitle)
	proposal := actions.Proposal(s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "Buy milk"}))

	msgs, recorded, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "add a task to buy milk"},
		{Role: chat.RoleAssistant, Content: "Proposed."},
	}, "add a task", []actions.NewAction{proposal})
	if err != nil || len(msgs) != 2 || len(recorded) != 1 {
		t.Fatalf("AppendTurn = %d messages, %d actions, %v", len(msgs), len(recorded), err)
	}
	if recorded[0].ConversationID == nil || *recorded[0].ConversationID != conv.ID || recorded[0].UserID != alice {
		t.Fatalf("recorded %+v", recorded[0])
	}

	// A turn that fails -- here, an invalid role on its second message -- takes
	// its proposal down with it.
	if _, _, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "and another"},
		{Role: "narrator", Content: "invalid"},
	}, "", []actions.NewAction{proposal}); err == nil {
		t.Fatal("an invalid turn was written")
	}
	// As does a turn whose action is invalid, messages included.
	if _, _, err := repo.AppendTurn(ctx, alice, conv.ID, []chat.NewMessage{
		{Role: chat.RoleUser, Content: "and another"}, {Role: chat.RoleAssistant, Content: "ok"},
	}, "", []actions.NewAction{{Call: proposal.Call, Status: actions.StatusExecuted, Result: map[string]any{}}}); err == nil {
		t.Fatal("a turn recording an executed write was written")
	}
	if n := countRows(t, s.pool, "actions"); n != 1 {
		t.Fatalf("%d actions after one good turn and two failed ones, want 1", n)
	}
	if n := countRows(t, s.pool, "messages"); n != 2 {
		t.Fatalf("%d messages after one good turn and two failed ones, want 2", n)
	}
	// And another user cannot record into this conversation.
	bob := makeUser(t, s.pool, "bob@example.com")
	if _, _, err := repo.AppendTurn(ctx, bob, conv.ID, []chat.NewMessage{{Role: chat.RoleUser, Content: "x"}},
		"", []actions.NewAction{proposal}); !errors.Is(err, chat.ErrNotFound) {
		t.Fatalf("bob's AppendTurn = %v, want ErrNotFound", err)
	}
}

func TestActionCascades(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	repo := chat.NewRepository(s.pool)
	conv := seedConversation(t, repo, alice, "Planning")
	a := s.record(t, alice, &conv.ID, actions.Proposal(s.prepare(t, alice, tools.CreateTask, tools.Args{"title": "x"})))

	// Deleting the conversation keeps the record, with no conversation.
	if err := repo.DeleteConversation(ctx, alice, conv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.actions.ByID(ctx, alice, a.ID)
	if err != nil || got.ConversationID != nil {
		t.Fatalf("after deleting the conversation: %+v, %v", got, err)
	}
	// Deleting the user takes it.
	if _, err := s.pool.Exec(`DELETE FROM users WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s.pool, "actions"); n != 0 {
		t.Fatalf("%d actions survived their user", n)
	}
}

// Two proposals are the same when they would do the same thing: jsonb
// equality, whatever order the keys were written in.
func TestPendingDuplicateComparesInputsAsJSON(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	alice := makeUser(t, s.pool, "alice@example.com")
	conv := seedConversation(t, chat.NewRepository(s.pool), alice, "Planning")
	call := tools.Call{Tool: tools.CreateTask, Permission: tools.Write, Input: json.RawMessage(`{"title":"Buy milk","priority":"high"}`)}
	a := s.record(t, alice, &conv.ID, actions.Proposal(call))

	reordered := call
	reordered.Input = json.RawMessage(`{"priority": "high", "title": "Buy milk"}`)
	if got, found, err := s.actions.PendingDuplicate(ctx, alice, conv.ID, reordered); err != nil || !found || got.ID != a.ID {
		t.Fatalf("PendingDuplicate = %+v, %v, %v", got, found, err)
	}
	other := call
	other.Input = json.RawMessage(`{"title":"Buy milk","priority":"low"}`)
	if _, found, _ := s.actions.PendingDuplicate(ctx, alice, conv.ID, other); found {
		t.Fatal("a different proposal counted as a duplicate")
	}
	if _, found, _ := s.actions.PendingDuplicate(ctx, makeUser(t, s.pool, "bob@example.com"), conv.ID, call); found {
		t.Fatal("another user's proposal counted as a duplicate")
	}
	if _, err := s.actions.Reject(ctx, alice, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.actions.PendingDuplicate(ctx, alice, conv.ID, call); found {
		t.Fatal("a rejected proposal counted as pending")
	}
}

// The text filter the search tools use, as SQL: case-insensitive, on title or
// the body column, owner-scoped, and literal -- `%` and `_` are characters.
func TestTextFiltersOnTasksGoalsAndNotes(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	alice := makeUser(t, pool, "alice@example.com")
	bob := makeUser(t, pool, "bob@example.com")
	taskRepo := tasks.NewRepository(pool)
	for _, in := range []tasks.CreateInput{
		{Title: "Fix the Antenna guy-line", Priority: "medium", Status: "pending"},
		{Title: "Order parts", Description: "a new antenna cable", Priority: "medium", Status: "pending"},
		{Title: "Budget: cut 50% of spend", Priority: "medium", Status: "pending"},
		{Title: "Buy milk", Priority: "medium", Status: "pending"},
	} {
		if _, err := taskRepo.Create(ctx, alice, in); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := taskRepo.Create(ctx, bob, tasks.CreateInput{Title: "Bob's antenna", Priority: "medium", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	find := func(q string) int {
		t.Helper()
		found, err := taskRepo.List(ctx, alice, tasks.Filter{Query: q, Sort: tasks.DefaultSort, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		return len(found)
	}
	for q, want := range map[string]int{"antenna": 2, "ANTENNA": 2, "50%": 1, "%": 1, "_": 0, "milk": 1, "passport": 0} {
		if got := find(q); got != want {
			t.Fatalf("q=%q found %d, want %d", q, got, want)
		}
	}

	goalRepo := goals.NewRepository(pool)
	if _, err := goalRepo.Create(ctx, alice, goals.CreateInput{Title: "Run a marathon", Description: "under four hours", Type: "personal", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if found, _ := goalRepo.List(ctx, alice, goals.Filter{Query: "four hours", Sort: goals.DefaultSort, Limit: 50}); len(found) != 1 {
		t.Fatalf("goal search found %d", len(found))
	}
	noteRepo := notes.NewRepository(pool)
	if _, err := noteRepo.Create(ctx, alice, notes.CreateInput{Title: "Cabin", Content: "the wifi password is hunter2"}); err != nil {
		t.Fatal(err)
	}
	if found, _ := noteRepo.List(ctx, alice, notes.Filter{Query: "WIFI", Sort: notes.DefaultSort, Limit: 50}); len(found) != 1 {
		t.Fatalf("note search found %d", len(found))
	}
	if found, _ := noteRepo.List(ctx, bob, notes.Filter{Query: "wifi", Sort: notes.DefaultSort, Limit: 50}); len(found) != 0 {
		t.Fatal("bob's search found alice's note")
	}
}
