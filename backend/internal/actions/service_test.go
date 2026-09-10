package actions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Approving runs the proposed write once and records what it produced.
func TestApproveExecutesTheProposalAndRecordsTheResult(t *testing.T) {
	e := newEngine()
	a := e.propose(e.user, "Renew the passport")
	if e.tasks.count() != 0 {
		t.Fatal("proposing created the task")
	}

	done, err := e.svc.Approve(context.Background(), e.user, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusExecuted || done.Result == nil || done.ErrorMessage != nil {
		t.Fatalf("action = %+v", done)
	}
	if e.tasks.count() != 1 || e.tasks.creates[0].Title != "Renew the passport" {
		t.Fatalf("creates = %+v", e.tasks.creates)
	}
	var result struct {
		Task struct{ Title string } `json:"task"`
	}
	if err := json.Unmarshal(done.Result, &result); err != nil || result.Task.Title != "Renew the passport" {
		t.Fatalf("result = %s", done.Result)
	}

	// Once. A second approve is refused and runs nothing.
	if _, err := e.svc.Approve(context.Background(), e.user, a.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a second approve = %v, want ErrNotPending", err)
	}
	if e.tasks.count() != 1 {
		t.Fatalf("approving twice created %d tasks", e.tasks.count())
	}
}

// Rejecting runs nothing, now or later.
func TestRejectRunsNothingEver(t *testing.T) {
	e := newEngine()
	a := e.propose(e.user, "Renew the passport")

	rejected, err := e.svc.Reject(context.Background(), e.user, a.ID)
	if err != nil || rejected.Status != StatusRejected {
		t.Fatalf("Reject = %+v, %v", rejected, err)
	}
	if _, err := e.svc.Approve(context.Background(), e.user, a.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving a rejected action = %v, want ErrNotPending", err)
	}
	if _, err := e.svc.Reject(context.Background(), e.user, a.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("rejecting twice = %v, want ErrNotPending", err)
	}
	if e.tasks.count() != 0 {
		t.Fatal("a rejected action ran")
	}
}

// Somebody else's action is not found -- whatever its state -- and nothing runs.
func TestAnotherUsersActionIsNotFound(t *testing.T) {
	e := newEngine()
	theirs := e.propose(uuid.New(), "Their task")

	for name, do := range map[string]func() error{
		"get":     func() error { _, err := e.svc.Get(context.Background(), e.user, theirs.ID); return err },
		"approve": func() error { _, err := e.svc.Approve(context.Background(), e.user, theirs.ID); return err },
		"reject":  func() error { _, err := e.svc.Reject(context.Background(), e.user, theirs.ID); return err },
	} {
		if err := do(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s = %v, want ErrNotFound", name, err)
		}
	}
	if e.tasks.count() != 0 || e.store.get(theirs.ID).Status != StatusProposed {
		t.Fatal("another user's action was touched")
	}
}

// A read cannot be approved: it was never pending.
func TestAReadIsNeverApprovable(t *testing.T) {
	e := newEngine()
	call, err := e.reg.Prepare(context.Background(), e.user, tools.SearchTasks, tools.Args{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	read := e.store.record(e.user, nil, Ran(call, tools.Result{Output: map[string]any{"count": 0}}))
	if _, err := e.svc.Approve(context.Background(), e.user, read.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approving a read = %v, want ErrNotPending", err)
	}
}

// A tool that fails on approval leaves the action failed with a sentence for
// the user -- never the underlying error, which can carry a DSN.
func TestAFailedExecutionIsRecordedWithoutLeakingTheError(t *testing.T) {
	e := newEngine()
	e.tasks.err = errors.New("dial tcp 10.0.0.7:5432: connection refused (postgres://admin:hunter2@db)")
	a := e.propose(e.user, "Renew the passport")

	done, err := e.svc.Approve(context.Background(), e.user, a.ID)
	if err != nil {
		t.Fatalf("a tool failure is an outcome, not a request error: %v", err)
	}
	if done.Status != StatusFailed || done.ErrorMessage == nil || done.Result != nil {
		t.Fatalf("action = %+v", done)
	}
	if msg := *done.ErrorMessage; strings.Contains(msg, "hunter2") || strings.Contains(msg, "5432") {
		t.Fatalf("error_message leaks the underlying error: %q", msg)
	}
}

// The approval is finished on a context the client cannot cancel: a client
// that hangs up the moment it clicks approve must not leave the row approved
// with the outcome unrecorded.
func TestAClientThatHangsUpDoesNotStrandAnApproval(t *testing.T) {
	e := newEngine()
	a := e.propose(e.user, "Renew the passport")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done, err := e.svc.Approve(ctx, e.user, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusExecuted {
		t.Fatalf("status = %s, want executed", done.Status)
	}
	if e.tasks.ctxErr != nil {
		t.Fatalf("the tool ran on a cancelled context: %v", e.tasks.ctxErr)
	}
}

// If the outcome cannot be recorded, the request fails -- and says so -- rather
// than reporting an action that looks untouched.
func TestAnOutcomeThatCannotBeRecordedIsAnError(t *testing.T) {
	e := newEngine()
	e.store.finishErr = errors.New("the database went away")
	a := e.propose(e.user, "Renew the passport")
	if _, err := e.svc.Approve(context.Background(), e.user, a.ID); err == nil {
		t.Fatal("a lost outcome was reported as success")
	}
}

func TestConcurrentApprovalsExecuteOnce(t *testing.T) {
	e := newEngine()
	a := e.propose(e.user, "Once")

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Approve(context.Background(), e.user, a.ID); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 || e.tasks.count() != 1 {
		t.Fatalf("%d approvals succeeded and %d tasks were created, want 1 and 1", won, e.tasks.count())
	}
}

// The state machine, on insert: a write is born proposed and nothing else; a
// read is born finished.
func TestNewActionsFollowTheStateMachine(t *testing.T) {
	write := tools.Call{Tool: tools.CreateTask, Permission: tools.Write, Input: json.RawMessage(`{"title":"x"}`)}
	read := tools.Call{Tool: tools.SearchTasks, Permission: tools.Read, Input: json.RawMessage(`{}`)}
	for name, n := range map[string]NewAction{
		"a write born executed": {Call: write, Status: StatusExecuted, Result: map[string]any{}},
		"a write born approved": {Call: write, Status: StatusApproved},
		"a write with a result": {Call: write, Status: StatusProposed, Result: map[string]any{}},
		"a read born proposed":  {Call: read, Status: StatusProposed},
		"a read with no result": {Call: read, Status: StatusExecuted},
		"a failed read, silent": {Call: read, Status: StatusFailed},
		"no permission":         {Call: tools.Call{Tool: "x", Input: json.RawMessage(`{}`)}, Status: StatusProposed},
		"no input":              {Call: tools.Call{Tool: tools.CreateTask, Permission: tools.Write}, Status: StatusProposed},
	} {
		if err := n.Validate(); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("%s: Validate = %v, want ErrInvalidState", name, err)
		}
	}
	for name, n := range map[string]NewAction{
		"a proposal":      Proposal(write),
		"a read that ran": Ran(read, tools.Result{Output: map[string]any{"count": 0}}),
		"a failed read":   {Call: read, Status: StatusFailed, ErrorMessage: "the search could not run"},
	} {
		if err := n.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestListFilterValidation(t *testing.T) {
	for name, f := range map[string]Filter{
		"status":     {Status: "done"},
		"permission": {Permission: "admin"},
		"limit":      {Limit: -1},
		"offset":     {Offset: -1},
	} {
		if _, err := ValidateFilter(f); err == nil {
			t.Fatalf("%s: accepted %+v", name, f)
		}
	}
	f, err := ValidateFilter(Filter{Limit: 10_000})
	if err != nil || f.Limit != MaxLimit {
		t.Fatalf("limit clamp = %+v, %v", f, err)
	}
}

// Recent is the conversation's writes, newest first -- never its reads, and
// never another conversation's.
func TestRecentIsThisConversationsWrites(t *testing.T) {
	e := newEngine()
	conv := uuid.New()
	other := uuid.New()
	call, _ := e.reg.Prepare(context.Background(), e.user, tools.CreateTask, tools.Args{"title": "a"})
	readCall, _ := e.reg.Prepare(context.Background(), e.user, tools.SearchTasks, tools.Args{})
	first := e.store.record(e.user, &conv, Proposal(call))
	e.store.record(e.user, &conv, Ran(readCall, tools.Result{Output: map[string]any{}}))
	e.store.record(e.user, &other, Proposal(call))
	second := e.store.record(e.user, &conv, Proposal(call))

	got, err := e.svc.Recent(context.Background(), e.user, conv, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("recent = %+v", got)
	}
}
