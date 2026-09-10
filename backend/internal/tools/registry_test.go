package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// --- the approval rule ----------------------------------------------------------

// The property this phase is not allowed to relax: a write tool runs only from
// an action the ledger has just moved from proposed to approved, and only
// once. Every way of asking without that approval runs nothing.
func TestAWriteToolNeverRunsWithoutAnApprovedAction(t *testing.T) {
	w := newWorld()
	ctx := context.Background()
	call, err := w.reg.Prepare(ctx, w.user, CreateTask, Args{"title": "Renew the passport"})
	if err != nil {
		t.Fatal(err)
	}
	if w.totalWrites() != 0 {
		t.Fatal("preparing a write wrote something")
	}

	// Asking for the write directly, even with a Call that claims to be a read,
	// is refused: the registry looks the permission up rather than trusting it.
	lying := call
	lying.Permission = Read
	if _, err := w.reg.RunRead(ctx, w.user, lying); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("RunRead(create_task) = %v, want ErrApprovalRequired", err)
	}

	// Every action the ledger will not approve runs nothing.
	stranger := uuid.New()
	for name, id := range map[string]uuid.UUID{
		"no such action":   uuid.New(),
		"somebody else's":  w.ledger.add(stranger, call, "proposed"),
		"already rejected": w.ledger.add(w.user, call, "rejected"),
		"already executed": w.ledger.add(w.user, call, "executed"),
		"already failed":   w.ledger.add(w.user, call, "failed"),
		// A row that already says approved -- by a crash, or by somebody
		// editing the table -- is not an approval. The gate is the transition
		// from proposed, which this row cannot make again.
		"already approved": w.ledger.add(w.user, call, "approved"),
	} {
		if _, err := w.reg.RunApproved(ctx, w.user, id); err == nil {
			t.Fatalf("%s: RunApproved ran", name)
		}
	}
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("%d writes happened without an approved action", n)
	}

	// The one that is approvable runs once, from the input the ledger holds.
	id := w.ledger.add(w.user, call, "proposed")
	exec, err := w.reg.RunApproved(ctx, w.user, id)
	if err != nil || exec.Err != nil {
		t.Fatalf("RunApproved = %v / %v", err, exec.Err)
	}
	if len(w.tasks.creates) != 1 || w.tasks.creates[0].Title != "Renew the passport" {
		t.Fatalf("creates = %+v, want the one approved task", w.tasks.creates)
	}
	// And never twice.
	if _, err := w.reg.RunApproved(ctx, w.user, id); !errors.Is(err, errLedgerNotPending) {
		t.Fatalf("a second RunApproved = %v, want the ledger's refusal", err)
	}
	if n := w.totalWrites(); n != 1 {
		t.Fatalf("%d writes after approving one action twice, want 1", n)
	}
}

// What runs is what the ledger approved. The caller names an action and
// nothing else, so there is no way to swap in a different input between the
// proposal the user saw and the execution.
func TestAnApprovedActionRunsTheStoredInputOnly(t *testing.T) {
	w := newWorld()
	stored := Call{Tool: CreateTask, Permission: Write, Input: json.RawMessage(`{"title":"What the user approved"}`)}
	id := w.ledger.add(w.user, stored, "proposed")

	if _, err := w.reg.RunApproved(context.Background(), w.user, id); err != nil {
		t.Fatal(err)
	}
	if w.tasks.creates[0].Title != "What the user approved" {
		t.Fatalf("ran %q", w.tasks.creates[0].Title)
	}
}

// A ledger row naming a read tool, or a tool that no longer exists, is
// approved-then-failed rather than run as something else.
func TestAnApprovedRowMustNameAWriteTool(t *testing.T) {
	w := newWorld()
	read := w.ledger.add(w.user, Call{Tool: SearchTasks, Input: json.RawMessage(`{}`)}, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, read)
	if err != nil || !errors.Is(exec.Err, ErrNotApprovable) {
		t.Fatalf("approving a read = %v / %v, want ErrNotApprovable", err, exec.Err)
	}
	if len(w.tasks.filters) != 0 {
		t.Fatal("the read tool ran from an approval")
	}

	gone := w.ledger.add(w.user, Call{Tool: "delete_task", Input: json.RawMessage(`{}`)}, "proposed")
	exec, err = w.reg.RunApproved(context.Background(), w.user, gone)
	if err != nil || !errors.Is(exec.Err, ErrUnknownTool) {
		t.Fatalf("approving an unknown tool = %v / %v, want ErrUnknownTool", err, exec.Err)
	}
}

// A stored input that does not decode strictly -- it has a key the tool's
// input type does not -- is refused rather than run with the key ignored.
func TestAStoredInputMustRoundTrip(t *testing.T) {
	w := newWorld()
	id := w.ledger.add(w.user, Call{Tool: CreateTask, Input: json.RawMessage(`{"title":"x","user_id":"someone else"}`)}, "proposed")
	exec, err := w.reg.RunApproved(context.Background(), w.user, id)
	if err != nil || exec.Err == nil {
		t.Fatalf("an input with an unknown key ran: %v / %v", err, exec.Err)
	}
	if w.totalWrites() != 0 {
		t.Fatal("it wrote")
	}
}

func TestARegistryWithoutALedgerCannotWrite(t *testing.T) {
	reg, err := NewRegistry(nil, Standard(Services{Tasks: newFakeTasks()})...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.RunApproved(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, ErrNoLedger) {
		t.Fatalf("err = %v, want ErrNoLedger", err)
	}
}

// Of many concurrent approvals of one action, exactly one runs -- provided the
// ledger keeps its contract, which internal/db pins against Postgres.
func TestConcurrentApprovalsRunOnce(t *testing.T) {
	w := newWorld()
	call, _ := w.reg.Prepare(context.Background(), w.user, CreateTask, Args{"title": "Once"})
	id := w.ledger.add(w.user, call, "proposed")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.reg.RunApproved(context.Background(), w.user, id)
		}()
	}
	wg.Wait()
	if n := w.totalWrites(); n != 1 {
		t.Fatalf("16 concurrent approvals wrote %d times, want 1", n)
	}
}

// Preparing a call never writes, for any tool -- including update_task, which
// reads to resolve its reference.
func TestPrepareNeverWrites(t *testing.T) {
	w := newWorld()
	w.tasks.seed(w.user, "Write the scheduler", "pending")
	for _, tc := range []struct {
		tool string
		args Args
	}{
		{CreateTask, Args{"title": "a"}},
		{UpdateTask, Args{"task": "scheduler", "status": "done"}},
		{CreateGoal, Args{"title": "b"}},
		{CreateNote, Args{"title": "c"}},
	} {
		if _, err := w.reg.Prepare(context.Background(), w.user, tc.tool, tc.args); err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
	}
	if n := w.totalWrites(); n != 0 {
		t.Fatalf("preparing wrote %d times", n)
	}
}

// --- declarations ---------------------------------------------------------------

func TestStandardToolsAreTheBriefsEight(t *testing.T) {
	w := newWorld()
	var reads, writes []string
	for _, tool := range w.reg.Tools() {
		switch tool.Permission {
		case Read:
			reads = append(reads, tool.Name)
		case Write:
			writes = append(writes, tool.Name)
		}
	}
	sort.Strings(reads)
	sort.Strings(writes)
	if want := []string{SearchDocuments, SearchGoals, SearchNotes, SearchTasks}; !slices.Equal(reads, want) {
		t.Fatalf("read tools = %v, want %v", reads, want)
	}
	if want := []string{CreateGoal, CreateNote, CreateTask, UpdateTask}; !slices.Equal(writes, want) {
		t.Fatalf("write tools = %v, want %v", writes, want)
	}
	// Deletion is deferred. Nothing may be registered that sounds like it.
	for _, tool := range w.reg.Tools() {
		for _, word := range []string{"delete", "remove", "destroy", "drop", "clear", "purge"} {
			if strings.Contains(tool.Name, word) {
				t.Fatalf("%s is registered; delete tools are deferred", tool.Name)
			}
		}
	}
	// Read tools come first: the routing prompt was measured in that order.
	if w.reg.Tools()[0].Permission != Read || w.reg.Tools()[len(w.reg.Tools())-1].Permission != Write {
		t.Fatal("the standard tools are not in read-then-write order")
	}
}

// Every tool's input and output schemas are real JSON Schema objects, and the
// input one is exactly its params.
func TestSchemasDescribeEveryTool(t *testing.T) {
	for _, tool := range newWorld().reg.Tools() {
		in := tool.InputSchema()
		if in.Type != "object" || len(in.Properties) != len(tool.Params) {
			t.Fatalf("%s input schema = %+v", tool.Name, in)
		}
		for _, r := range in.Required {
			if _, ok := in.Properties[r]; !ok {
				t.Fatalf("%s requires %q, which it does not declare", tool.Name, r)
			}
		}
		if tool.Output.Type != "object" || len(tool.Output.Properties) == 0 {
			t.Fatalf("%s has no output schema", tool.Name)
		}
		raw, err := json.Marshal(in)
		if err != nil || !strings.Contains(string(raw), `"type":"object"`) {
			t.Fatalf("%s input schema does not marshal: %s %v", tool.Name, raw, err)
		}
	}
}

// The canonical input a tool stores never carries a key its schema does not
// declare -- so what the user is shown, what the schema promises and what runs
// are one document.
func TestCanonicalInputOnlyCarriesDeclaredParams(t *testing.T) {
	w := newWorld()
	w.tasks.seed(w.user, "Write the scheduler", "pending")
	everything := Args{
		"title": "t", "description": "d", "priority": "high", "deadline": "tomorrow",
		"category": "c", "tags": []any{"a"}, "query": "q", "status": "completed", "tag": "x",
		"type": "career", "content": "body", "task": "scheduler",
		"user_id": uuid.NewString(), "id": "ignored",
	}
	for _, tool := range w.reg.Tools() {
		call, err := w.reg.Prepare(context.Background(), w.user, tool.Name, everything)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		var keys map[string]any
		if err := json.Unmarshal(call.Input, &keys); err != nil {
			t.Fatal(err)
		}
		declared := map[string]bool{}
		for _, p := range tool.Params {
			declared[p.Name] = true
		}
		for k := range keys {
			if !declared[k] {
				t.Fatalf("%s stored the undeclared key %q: %s", tool.Name, k, call.Input)
			}
		}
		if call.Summary == "" || !strings.HasSuffix(call.Summary, ".") {
			t.Fatalf("%s summary = %q", tool.Name, call.Summary)
		}
	}
}

func TestNewRegistryRejectsBadDeclarations(t *testing.T) {
	good := Standard(Services{})[0]
	for name, ts := range map[string][]Tool{
		"duplicate":       {good, good},
		"not snake_case":  {func() Tool { t := good; t.Name = "Search-Tasks"; return t }()},
		"no description":  {func() Tool { t := good; t.Description = ""; return t }()},
		"no permission":   {func() Tool { t := good; t.Permission = "admin"; return t }()},
		"not from define": {{Name: "raw_tool", Description: "x", Permission: Read}},
	} {
		if _, err := NewRegistry(nil, ts...); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestDescribeSurvivesAMissingToolOrABadInput(t *testing.T) {
	w := newWorld()
	if got := w.reg.Describe("delete_everything", json.RawMessage(`{}`)); !strings.Contains(got, "no longer has") {
		t.Fatalf("Describe(unknown) = %q", got)
	}
	if got := w.reg.Describe(CreateTask, json.RawMessage(`{"nope":1}`)); !strings.Contains(got, "could not be read") {
		t.Fatalf("Describe(bad input) = %q", got)
	}
}

func TestPrepareRejectsAnUnknownTool(t *testing.T) {
	if _, err := newWorld().reg.Prepare(context.Background(), uuid.New(), "delete_task", nil); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
}
