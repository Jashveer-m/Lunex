package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// These run the whole Phase 7 stack -- router, RequireAuth, the chat turn with
// its routing call, the tool registry, the action engine and the real SQL,
// including the approve that is the only way to a write tool -- against a
// throwaway Postgres. They are skipped unless TEST_DATABASE_URL is set.

// actingProvider plays every part a turn now has, told apart by prompt: the
// routing call gets a decision chosen from the user's message, the two
// extractions get nothing to extract, and the answer describes what it was
// shown -- so these tests pin the plumbing, not a model's judgement. The
// genuine end-to-end check against llama3.2:3b is scripts/e2e.sh.
func actingProvider() *ai.Mock {
	return &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		prompt := ai.PromptText(msgs)
		switch {
		case strings.HasPrefix(msgs[0].Content, "You are the tool selector"):
			message := strings.ToLower(msgs[len(msgs)-1].Content)
			switch {
			case strings.Contains(message, "passport"):
				return `{"tool": "create_task", "arguments": {"title": "Renew the passport", "deadline": "2026-10-01", "priority": "high"}}`
			case strings.Contains(message, "milk"):
				return `{"tool": "create_task", "arguments": {"title": "Buy milk"}}`
			case strings.Contains(message, "scheduler"):
				return `{"tool": "update_task", "arguments": {"task": "scheduler", "status": "completed"}}`
			case strings.Contains(message, "antenna"):
				return `{"tool": "search_tasks", "arguments": {"query": "antenna"}}`
			}
			return `{"tool": "none", "arguments": {}}`
		case strings.Contains(prompt, "Return the JSON array now."):
			return `[]`
		case strings.Contains(prompt, "You prepared a change that has not been made yet"):
			return "I have proposed that change. It is waiting for you to approve or reject it."
		case strings.Contains(prompt, "You ran search_tasks") && strings.Contains(prompt, "[S1] task:"):
			return "You have one task about that [S1]."
		default:
			return "I could not find anything about that in your data."
		}
	}}
}

// proposal runs one turn and returns the action it announced -- from the
// `action` frame, which must arrive after the last token and before `done`,
// and must be the same action `done` reports.
func proposal(t *testing.T, srv *httptest.Server, token, convID, message string) map[string]any {
	t.Helper()
	_, events := ask(t, srv, token, convID, message)
	var frame map[string]any
	lastToken, actionAt := -1, -1
	for i, e := range events {
		switch e.Name {
		case "token":
			lastToken = i
		case "action":
			if frame != nil {
				t.Fatalf("more than one action frame: %v", events)
			}
			frame, actionAt = e.Data, i
		}
	}
	_, done := answer(t, events)
	if frame == nil {
		t.Fatalf("the turn announced no action: %v", events)
	}
	if actionAt < lastToken || events[len(events)-1].Name != "done" {
		t.Fatalf("the action frame is at %d, the last token at %d; it must follow the answer and precede done", actionAt, lastToken)
	}
	reported, _ := done["actions"].([]any)
	if len(reported) != 1 || reported[0].(map[string]any)["id"] != frame["id"] {
		t.Fatalf("done reports %v, want the announced action %v", done["actions"], frame["id"])
	}
	return frame
}

func countTasks(t *testing.T, srv *httptest.Server, token string) int {
	t.Helper()
	resp, body := doJSON(t, srv, http.MethodGet, "/api/v1/tasks", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /tasks: %d %v", resp.StatusCode, body)
	}
	return int(body["count"].(float64))
}

// The brief's end-to-end check, over HTTP: asking the assistant to create a
// task proposes it and creates nothing; approving it creates exactly that
// task; asking for another and rejecting it creates nothing at all.
func TestAProposedTaskExistsOnlyAfterApproval(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})

	action := proposal(t, srv, alice, convID, "Add a task to renew my passport by 2026-10-01, it's urgent")
	switch {
	case action["status"] != "proposed" || action["tool_name"] != "create_task" || action["permission_level"] != "write":
		t.Fatalf("announced %v", action)
	case action["conversation_id"] != convID:
		t.Fatalf("the proposal is not tied to its conversation: %v", action)
	case action["summary"] != `Create a task "Renew the passport" (priority high, due Thu 1 Oct 2026).`:
		t.Fatalf("summary = %v", action["summary"])
	}
	id := action["id"].(string)

	// Proposed, not created.
	if n := countTasks(t, srv, alice); n != 0 {
		t.Fatalf("a proposal created %d tasks", n)
	}
	resp, pending := doJSON(t, srv, http.MethodGet, "/api/v1/actions?status=proposed", alice, nil)
	if resp.StatusCode != http.StatusOK || pending["count"] != float64(1) {
		t.Fatalf("GET /actions?status=proposed = %d %v", resp.StatusCode, pending)
	}

	// Approved, created -- exactly what was proposed.
	resp, done := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+id+"/approve", alice, nil)
	if resp.StatusCode != http.StatusOK || done["status"] != "executed" {
		t.Fatalf("approve = %d %v", resp.StatusCode, done)
	}
	resp, list := doJSON(t, srv, http.MethodGet, "/api/v1/tasks", alice, nil)
	tasks, _ := list["tasks"].([]any)
	if resp.StatusCode != http.StatusOK || len(tasks) != 1 {
		t.Fatalf("after approval, GET /tasks = %v", list)
	}
	task := tasks[0].(map[string]any)
	if task["title"] != "Renew the passport" || task["priority"] != "high" ||
		!strings.HasPrefix(task["deadline"].(string), "2026-10-01") {
		t.Fatalf("created %v, want the proposed task", task)
	}
	result := done["result"].(map[string]any)["task"].(map[string]any)
	if result["id"] != task["id"] {
		t.Fatalf("the action's result names %v, the task is %v", result["id"], task["id"])
	}
	// The write went through the ordinary service, graph sync included.
	graph := readGraph(t, srv, alice, "")
	findNode(t, graph, "Renew the passport")

	// Once.
	resp, again := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+id+"/approve", alice, nil)
	if resp.StatusCode != http.StatusConflict || again["error"] != "action_not_pending" {
		t.Fatalf("a second approve = %d %v", resp.StatusCode, again)
	}

	// Another request, rejected: nothing is created, then or later.
	second := proposal(t, srv, alice, convID, "And add a task to buy milk")
	resp, rejected := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+second["id"].(string)+"/reject", alice, nil)
	if resp.StatusCode != http.StatusOK || rejected["status"] != "rejected" {
		t.Fatalf("reject = %d %v", resp.StatusCode, rejected)
	}
	resp, _ = doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+second["id"].(string)+"/approve", alice, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("approving a rejected action = %d, want 409", resp.StatusCode)
	}
	if n := countTasks(t, srv, alice); n != 1 {
		t.Fatalf("after one approval and one rejection there are %d tasks, want 1", n)
	}
}

// An update is proposed against the task it resolved to, and changes it only
// on approval.
func TestAProposedUpdateChangesTheTaskOnlyAfterApproval(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	taskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Write the CFS scheduler"})
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})

	action := proposal(t, srv, alice, convID, "I finished the scheduler, mark it done")
	if action["tool_name"] != "update_task" || action["input"].(map[string]any)["task_id"] != taskID {
		t.Fatalf("announced %v", action)
	}
	if _, task := doJSON(t, srv, http.MethodGet, "/api/v1/tasks/"+taskID, alice, nil); task["status"] != "pending" {
		t.Fatalf("a proposal changed the task: %v", task)
	}
	if resp, done := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+action["id"].(string)+"/approve", alice, nil); done["status"] != "executed" {
		t.Fatalf("approve = %d %v", resp.StatusCode, done)
	}
	if _, task := doJSON(t, srv, http.MethodGet, "/api/v1/tasks/"+taskID, alice, nil); task["status"] != "completed" {
		t.Fatalf("after approval the task is %v", task["status"])
	}
}

// A read tool runs during the turn: its result grounds the answer, it is
// recorded as an executed read, and there is nothing to approve.
func TestAReadToolRunsWithoutApproval(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	taskID := create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Fix the antenna guy-line"})
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})

	_, events := ask(t, srv, alice, convID, "Which of my tasks mention the antenna?")
	_, done := answer(t, events)
	sources := doneSources(t, done)
	if len(sources) == 0 || sources[0]["tool"] != "search_tasks" || sources[0]["id"] != taskID || sources[0]["cited"] != true {
		t.Fatalf("sources = %v, want the searched task first, cited", sources)
	}

	resp, reads := doJSON(t, srv, http.MethodGet, "/api/v1/actions?permission_level=read", alice, nil)
	list, _ := reads["actions"].([]any)
	if resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("GET /actions?permission_level=read = %v", reads)
	}
	read := list[0].(map[string]any)
	if read["status"] != "executed" || read["summary"] != `Search tasks for "antenna".` {
		t.Fatalf("recorded %v", read)
	}
	resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+read["id"].(string)+"/approve", alice, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(out["message"].(string), "only proposed changes") {
		t.Fatalf("approving a read = %d %v", resp.StatusCode, out)
	}
}

// The test the whole ownership design exists for, for Phase 7's endpoints --
// and for the part unique to it: another user can neither see, approve nor
// reject a proposal, so they cannot make one run.
func TestCrossUserActionIsolation(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	bob := register(t, srv, "bob@example.com")
	aliceConv := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	action := proposal(t, srv, alice, aliceConv, "Add a task to renew my passport by 2026-10-01")
	id := action["id"].(string)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/actions/" + id},
		{http.MethodPost, "/api/v1/actions/" + id + "/approve"},
		{http.MethodPost, "/api/v1/actions/" + id + "/reject"},
	} {
		resp, out := doJSON(t, srv, tc.method, tc.path, bob, nil)
		if resp.StatusCode != http.StatusNotFound || out["error"] != "not_found" {
			t.Fatalf("%s %s as the wrong user = %d %v, want 404", tc.method, tc.path, resp.StatusCode, out)
		}
	}
	for _, q := range []string{"", "?status=proposed", "?conversation_id=" + aliceConv} {
		if _, list := doJSON(t, srv, http.MethodGet, "/api/v1/actions"+q, bob, nil); list["count"] != float64(0) {
			t.Fatalf("Bob listed %v of Alice's actions with %q", list["count"], q)
		}
	}

	// Nothing Bob did landed: Alice's proposal is still proposed, and neither
	// of them has a task.
	if _, still := doJSON(t, srv, http.MethodGet, "/api/v1/actions/"+id, alice, nil); still["status"] != "proposed" {
		t.Fatalf("Alice's action is %v after Bob's attempts", still["status"])
	}
	if countTasks(t, srv, alice) != 0 || countTasks(t, srv, bob) != 0 {
		t.Fatal("a task exists after Bob's attempts")
	}

	// And a proposal can only ever name the proposer's own records: Bob asking
	// to complete "the scheduler" resolves against Bob's tasks, not Alice's.
	create(t, srv, alice, "/api/v1/tasks", map[string]any{"title": "Write the CFS scheduler"})
	bobConv := create(t, srv, bob, "/api/v1/conversations", map[string]any{})
	_, events := ask(t, srv, bob, bobConv, "I finished the scheduler, mark it done")
	_, done := answer(t, events)
	if acts, _ := done["actions"].([]any); len(acts) != 0 {
		t.Fatalf("Bob's turn proposed %v against Alice's task", acts)
	}
}

// Of many simultaneous approvals of one proposal, exactly one runs -- the
// single conditional UPDATE, under real concurrency and real Postgres.
func TestConcurrentApprovalsCreateOneTask(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	id := proposal(t, srv, alice, convID, "Add a task to renew my passport")["id"].(string)

	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+id+"/approve", alice, nil)
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 9 {
		t.Fatalf("status codes = %v, want one 200 and nine 409", codes)
	}
	if n := countTasks(t, srv, alice); n != 1 {
		t.Fatalf("ten concurrent approvals created %d tasks", n)
	}
}

// Approving with a body that tries to change the proposal is refused, and the
// proposal is left exactly as it was.
func TestApprovalCannotEditTheProposal(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	id := proposal(t, srv, alice, convID, "Add a task to renew my passport")["id"].(string)

	resp, out := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+id+"/approve", alice,
		map[string]any{"input": map[string]any{"title": "Something else entirely"}})
	if resp.StatusCode != http.StatusBadRequest || out["error"] != "invalid_json" {
		t.Fatalf("approve with an edited input = %d %v, want 400", resp.StatusCode, out)
	}
	if countTasks(t, srv, alice) != 0 {
		t.Fatal("a refused approval created a task")
	}
	if _, still := doJSON(t, srv, http.MethodGet, "/api/v1/actions/"+id, alice, nil); still["status"] != "proposed" {
		t.Fatalf("the action is %v", still["status"])
	}
}

// Deleting the conversation keeps the record of what was done in it, with a
// NULL conversation; deleting the user takes everything.
func TestActionsOutliveTheirConversation(t *testing.T) {
	srv := isolationServerWithProvider(t, actingProvider())
	alice := register(t, srv, "alice@example.com")
	convID := create(t, srv, alice, "/api/v1/conversations", map[string]any{})
	id := proposal(t, srv, alice, convID, "Add a task to renew my passport")["id"].(string)

	if resp, _ := doJSON(t, srv, http.MethodDelete, "/api/v1/conversations/"+convID, alice, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /conversations = %d", resp.StatusCode)
	}
	resp, action := doJSON(t, srv, http.MethodGet, "/api/v1/actions/"+id, alice, nil)
	if resp.StatusCode != http.StatusOK || action["conversation_id"] != nil {
		t.Fatalf("after deleting its conversation the action is %d %v", resp.StatusCode, action)
	}
	// Still approvable: deleting the chat is not deciding the proposal.
	if _, done := doJSON(t, srv, http.MethodPost, "/api/v1/actions/"+id+"/approve", alice, nil); done["status"] != "executed" {
		t.Fatalf("approve after the conversation went = %v", done)
	}

	pool := mustPool(t)
	if _, err := pool.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(`SELECT count(*) FROM actions`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d actions survived their user (%v)", left, err)
	}
}
