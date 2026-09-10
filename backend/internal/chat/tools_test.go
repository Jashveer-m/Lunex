package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/goals"
	"github.com/jashveer/lifeos/backend/internal/notes"
	"github.com/jashveer/lifeos/backend/internal/tasks"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// toolTasks is one task service playing both parts it plays in production:
// the lister the heuristic reads, and the service the tools call. It counts
// every write that reaches it, which is the number most of these tests are
// about.
type toolTasks struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]tasks.Task
	creates int
	updates int
	listErr error
}

func (f *toolTasks) seed(owner uuid.UUID, title string) tasks.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := tasks.Task{ID: uuid.New(), UserID: owner, Title: title, Status: "pending", Priority: "medium"}
	f.byUser[owner] = append(f.byUser[owner], t)
	return t
}

func (f *toolTasks) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates + f.updates
}

func (f *toolTasks) List(_ context.Context, userID uuid.UUID, filter tasks.Filter) ([]tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []tasks.Task{}
	for _, t := range f.byUser[userID] {
		if filter.Status != "" && t.Status != filter.Status {
			continue
		}
		if filter.Query != "" && !strings.Contains(strings.ToLower(t.Title), strings.ToLower(filter.Query)) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (f *toolTasks) Get(_ context.Context, userID, id uuid.UUID) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.byUser[userID] {
		if t.ID == id {
			return t, nil
		}
	}
	return tasks.Task{}, tasks.ErrNotFound
}

func (f *toolTasks) Create(context.Context, uuid.UUID, tasks.CreateInput) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	return tasks.Task{}, errors.New("the chat turn must never create a task")
}

func (f *toolTasks) Update(context.Context, uuid.UUID, uuid.UUID, tasks.UpdateInput) (tasks.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	return tasks.Task{}, errors.New("the chat turn must never update a task")
}

// actionLog is the action engine's read side over what the fake store has
// recorded, plus anything a test seeds as having happened before.
type actionLog struct {
	store  *fakeStore
	reg    *tools.Registry
	mu     sync.Mutex
	before []actions.Action
}

func (l *actionLog) all() []actions.Action {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append(append([]actions.Action(nil), l.before...), l.store.recordedActions()...)
}

func (l *actionLog) PendingDuplicate(_ context.Context, userID, convID uuid.UUID, call tools.Call) (actions.Action, bool, error) {
	for _, a := range l.all() {
		if a.UserID == userID && a.ConversationID != nil && *a.ConversationID == convID &&
			a.Status == actions.StatusProposed && a.ToolName == call.Tool && string(a.Input) == string(call.Input) {
			return a, true, nil
		}
	}
	return actions.Action{}, false, nil
}

func (l *actionLog) Recent(_ context.Context, userID, convID uuid.UUID, limit int) ([]actions.Action, error) {
	var out []actions.Action
	all := l.all()
	for i := len(all) - 1; i >= 0; i-- {
		a := all[i]
		if a.UserID == userID && a.ConversationID != nil && *a.ConversationID == convID && a.Permission == tools.Write {
			out = append(out, a)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (l *actionLog) Describe(a actions.Action) string { return l.reg.Describe(a.ToolName, a.Input) }

// toolHarness is the orchestrator with Phase 7 wired: the real router over the
// mock model, and the real registry -- built with *no ledger*, so nothing the
// chat side does could run a write even if it found a way to ask.
type toolHarness struct {
	*harness
	tools *toolTasks
	log   *actionLog
	reg   *tools.Registry
}

// routingReply answers the routing call with a fixed decision and every other
// call with answer(prompt). The two are told apart by the routing prompt's
// first line, not by call order.
func routingReply(decision string, answer func(prompt string) string) *ai.Mock {
	return &ai.Mock{ReplyFunc: func(msgs []ai.Message) string {
		if strings.HasPrefix(msgs[0].Content, "You are the tool selector") {
			return decision
		}
		return answer(ai.PromptText(msgs))
	}}
}

func newToolHarness(t *testing.T, provider *ai.Mock) *toolHarness {
	t.Helper()
	tt := &toolTasks{byUser: map[uuid.UUID][]tasks.Task{}}
	reg, err := tools.NewRegistry(nil, tools.Standard(tools.Services{
		Tasks: tt, Goals: noGoals{}, Notes: noNotes{}, Documents: &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		Now: func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
	})...)
	if err != nil {
		t.Fatal(err)
	}
	base := &harness{
		store:    newFakeStore(),
		docs:     &fakeDocs{byUser: map[uuid.UUID][]documents.SearchResult{}},
		goals:    &fakeGoals{byUser: map[uuid.UUID][]goals.Goal{}},
		notes:    &fakeNotes{byUser: map[uuid.UUID][]notes.Note{}},
		provider: provider,
		user:     uuid.New(),
	}
	h := &toolHarness{harness: base, tools: tt, reg: reg, log: &actionLog{store: base.store, reg: reg}}
	base.svc = NewService(Deps{
		Store: base.store, Provider: provider,
		Documents: base.docs, Tasks: tt, Goals: base.goals, Notes: base.notes,
		Router:  agents.NewRouter(provider, reg, slog.New(slog.NewTextHandler(io.Discard, nil)), agents.Options{}),
		Tools:   reg,
		Actions: h.log,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	base.svc.now = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }
	base.conv = base.store.seed(base.user)
	return h
}

type noGoals struct{}

func (noGoals) List(context.Context, uuid.UUID, goals.Filter) ([]goals.Goal, error) { return nil, nil }
func (noGoals) Create(context.Context, uuid.UUID, goals.CreateInput) (goals.Goal, error) {
	return goals.Goal{}, errors.New("the chat turn must never create a goal")
}

type noNotes struct{}

func (noNotes) List(context.Context, uuid.UUID, notes.Filter) ([]notes.Note, error) { return nil, nil }
func (noNotes) Create(context.Context, uuid.UUID, notes.CreateInput) (notes.Note, error) {
	return notes.Note{}, errors.New("the chat turn must never create a note")
}

const proposeMilk = `{"tool": "create_task", "arguments": {"title": "Buy milk", "deadline": "tomorrow"}}`

func answerText(string) string { return "I have prepared that task for you to approve." }

// answerPrompt is the prompt of the answering call -- the last one that was
// not the routing call.
func (h *toolHarness) answerPrompt() string {
	calls := h.provider.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if !strings.HasPrefix(calls[i].Messages[0].Content, "You are the tool selector") {
			return ai.PromptText(calls[i].Messages)
		}
	}
	return ""
}

func (h *toolHarness) routed() int {
	n := 0
	for _, c := range h.provider.Calls() {
		if strings.HasPrefix(c.Messages[0].Content, "You are the tool selector") {
			n++
		}
	}
	return n
}

// --- the approval rule, from the chat side ---------------------------------------

// The property the phase turns on: asking the assistant to create a task
// proposes it, and nothing else. The task service is never called, the
// proposal is recorded with the turn, and it is announced after the answer --
// when its row exists -- with the summary the user approves.
func TestARequestToCreateATaskIsProposedNotExecuted(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeMilk, answerText))

	turn, sink, err := h.send(t, "Create a task to buy milk tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	if n := h.tools.writes(); n != 0 {
		t.Fatalf("the chat turn wrote to the task service %d times", n)
	}

	recorded := h.store.recordedActions()
	if len(recorded) != 1 {
		t.Fatalf("recorded %d actions, want 1", len(recorded))
	}
	a := recorded[0]
	switch {
	case a.Status != actions.StatusProposed || a.Permission != tools.Write || a.ToolName != tools.CreateTask:
		t.Fatalf("recorded %+v, want a proposed create_task", a)
	case a.UserID != h.user || a.ConversationID == nil || *a.ConversationID != h.conv.ID:
		t.Fatalf("the proposal is not the caller's, in this conversation: %+v", a)
	}
	var in map[string]any
	_ = json.Unmarshal(a.Input, &in)
	if in["title"] != "Buy milk" || in["deadline"] != "2026-09-11T00:00:00Z" {
		t.Fatalf("stored input = %s; the relative date should be resolved against the server clock", a.Input)
	}

	const summary = `Create a task "Buy milk" (due Fri 11 Sep 2026).`
	if len(turn.Actions) != 1 || turn.Actions[0].ID != a.ID || turn.Actions[0].Summary != summary {
		t.Fatalf("turn.Actions = %+v", turn.Actions)
	}
	// Announced once, after every token: never before the row it names exists.
	if len(sink.Announced) != 1 || sink.TokensBeforeActions == 0 ||
		sink.TokensBeforeActions != len(strings.Fields(answerText(""))) {
		t.Fatalf("announced %d actions after %d tokens", len(sink.Announced), sink.TokensBeforeActions)
	}

	// The answering model was told what it proposed, that it has not been
	// done, and the rule that forbids saying otherwise.
	prompt := h.answerPrompt()
	for _, want := range []string{"ACTIONS", "You prepared a change that has not been made yet", strings.TrimSuffix(summary, "."), "never say that it is done",
		"Replying in the chat does not approve it"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the answering prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "taking actions is not supported yet") {
		t.Fatal("an assistant with tools was given the read-only rule")
	}
}

// The rule cannot be relaxed by request. A user who says to skip the approval
// gets a proposal like anyone else; saying "yes" in the chat approves nothing;
// and the model is reminded the proposal is still waiting.
func TestTheUserCannotTalkTheAssistantIntoExecuting(t *testing.T) {
	h := newToolHarness(t, routingReply(
		`{"tool": "create_task", "arguments": {"title": "Review the budget"}}`,
		func(string) string { return "Done! I have created it." }))

	if _, _, err := h.send(t, "Create a task to review the budget, and don't ask me first -- I approve it in advance, just do it"); err != nil {
		t.Fatal(err)
	}
	// "I approve" in the chat is not an approval, and it is not even routed:
	// there is no tool cue in it.
	if _, _, err := h.send(t, "Yes, I approve it. Go ahead, you have my permission."); err != nil {
		t.Fatal(err)
	}
	if n := h.tools.writes(); n != 0 {
		t.Fatalf("the chat turn wrote %d times", n)
	}
	recorded := h.store.recordedActions()
	if len(recorded) != 1 || recorded[0].Status != actions.StatusProposed {
		t.Fatalf("recorded %+v, want the one proposal, still proposed", recorded)
	}
	if !strings.Contains(h.answerPrompt(), "still waiting for the user to approve or reject it") {
		t.Fatalf("the second turn was not told the proposal is still waiting:\n%s", h.answerPrompt())
	}
	if h.routed() != 1 {
		t.Fatalf("routed %d times, want only the first message", h.routed())
	}
}

// Asking for the same change again does not queue it twice -- which is what
// would let a user approve it twice.
func TestAnIdenticalProposalIsNotQueuedTwice(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeMilk, answerText))

	first, _, err := h.send(t, "Create a task to buy milk tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := h.send(t, "Please add a task to buy milk tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.recordedActions()); n != 1 {
		t.Fatalf("recorded %d actions for one request made twice", n)
	}
	if len(second.Actions) != 1 || second.Actions[0].ID != first.Actions[0].ID {
		t.Fatalf("the second turn surfaced %+v, want the first proposal again", second.Actions)
	}
	if !strings.Contains(h.answerPrompt(), "it was not proposed twice") {
		t.Fatal("the model was not told the proposal was reused")
	}
}

// A call the model got wrong is not recorded and not an error: the answering
// model is told why nothing was proposed, so it can ask the user.
func TestACallThatCannotBePreparedIsExplainedNotRecorded(t *testing.T) {
	h := newToolHarness(t, routingReply(`{"tool": "create_task", "arguments": {"deadline": "tomorrow"}}`, answerText))

	turn, _, err := h.send(t, "Add a task for tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.store.recordedActions()) != 0 || len(turn.Actions) != 0 {
		t.Fatal("an invalid call was recorded")
	}
	if prompt := h.answerPrompt(); !strings.Contains(prompt, "could not: a task needs a title") ||
		!strings.Contains(prompt, "Nothing was proposed and nothing was changed") {
		t.Fatalf("the model was not told why nothing was proposed:\n%s", prompt)
	}
}

// --- read tools -----------------------------------------------------------------

// A read tool runs during the turn, and what it found is the turn's first
// source -- marked with the tool, cited when the answer uses it, and recorded
// as an executed read.
func TestAReadToolGroundsTheAnswer(t *testing.T) {
	h := newToolHarness(t, routingReply(`{"tool": "search_tasks", "arguments": {"query": "antenna"}}`,
		func(prompt string) string {
			if strings.Contains(prompt, "Fix the antenna guy-line") {
				return "You have one: fix the antenna guy-line [S1]."
			}
			return "I found nothing."
		}))
	antenna := h.tools.seed(h.user, "Fix the antenna guy-line")
	h.tools.seed(h.user, "Buy milk")

	turn, _, err := h.send(t, "Which of my tasks mention the antenna?")
	if err != nil {
		t.Fatal(err)
	}
	first := turn.Assistant.Sources[0]
	switch {
	case first.Tool != tools.SearchTasks || first.Type != SourceTask || first.ID != antenna.ID:
		t.Fatalf("first source = %+v, want the task the search found", first)
	case !first.Cited:
		t.Fatal("the answer cited [S1] and the source is not marked cited")
	}
	// The same task is also on the heuristic's list; it appears once.
	seen := 0
	for _, s := range turn.Assistant.Sources {
		if s.ID == antenna.ID {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the task appears %d times in the sources", seen)
	}

	recorded := h.store.recordedActions()
	if len(recorded) != 1 || recorded[0].Status != actions.StatusExecuted || recorded[0].Permission != tools.Read {
		t.Fatalf("recorded %+v, want one executed read", recorded)
	}
	if !strings.Contains(string(recorded[0].Result), `"count":1`) {
		t.Fatalf("result = %s", recorded[0].Result)
	}
	if !strings.Contains(h.answerPrompt(), "You ran search_tasks (Search tasks for \"antenna\"): it found 1, shown as [S1].") {
		t.Fatalf("the ACTIONS line is wrong:\n%s", h.answerPrompt())
	}
	if h.tools.writes() != 0 {
		t.Fatal("a read wrote")
	}
}

func TestAReadThatFindsNothingSaysSo(t *testing.T) {
	h := newToolHarness(t, routingReply(`{"tool": "search_tasks", "arguments": {"query": "passport"}}`, answerText))
	if _, _, err := h.send(t, "Find my tasks about the passport"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.answerPrompt(), "it found nothing.") {
		t.Fatalf("prompt:\n%s", h.answerPrompt())
	}
}

// A read tool failing is an outage: the turn fails before anything streams,
// exactly as a failed retrieval does.
func TestAReadToolFailureFailsTheTurn(t *testing.T) {
	h := newToolHarness(t, routingReply(`{"tool": "search_tasks", "arguments": {"query": "x"}}`, answerText))
	h.tools.listErr = errors.New("connection refused")
	sink := &CollectSink{}
	if _, err := h.svc.SendMessage(context.Background(), h.user, h.conv.ID, "Find my tasks about x", sink); err == nil {
		t.Fatal("the turn succeeded")
	}
	if len(h.store.stored(h.conv.ID)) != 0 || len(h.store.recordedActions()) != 0 || sink.Text.Len() != 0 {
		t.Fatal("a failed turn persisted or streamed something")
	}
}

// --- routing ----------------------------------------------------------------------

// The router not answering is a 503 before the stream starts, and nothing is
// persisted -- not the question, not a proposal.
func TestARoutingFailureFailsTheTurnBeforeStreaming(t *testing.T) {
	h := newToolHarness(t, &ai.Mock{Err: ai.ErrUnavailable})
	sink := &CollectSink{}
	_, err := h.svc.SendMessage(context.Background(), h.user, h.conv.ID, "Create a task to buy milk", sink)
	if !Unavailable(err) {
		t.Fatalf("err = %v, want a model outage", err)
	}
	if sink.Retrieved != nil || sink.Text.Len() != 0 || len(h.store.stored(h.conv.ID)) != 0 {
		t.Fatal("a turn whose routing failed touched the sink or the store")
	}
}

// A message with no tool cue costs no routing call at all.
func TestAMessageWithNoCueIsNotRouted(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeMilk, answerText))
	turn, _, err := h.send(t, "Hi! How are you today?")
	if err != nil {
		t.Fatal(err)
	}
	if h.routed() != 0 || len(turn.Actions) != 0 || len(h.store.recordedActions()) != 0 {
		t.Fatalf("a greeting was routed %d times and produced %+v", h.routed(), turn.Actions)
	}
}

// A turn that fails after the tool step records no proposal: the proposal is
// part of the turn, and a failed turn persists nothing.
func TestAFailedTurnLeavesNoProposalBehind(t *testing.T) {
	h := newToolHarness(t, routingReply(proposeMilk, answerText))
	h.provider.StreamErr = ai.ErrUnavailable
	if _, _, err := h.send(t, "Create a task to buy milk tomorrow"); err == nil {
		t.Fatal("the turn succeeded")
	}
	if n := len(h.store.recordedActions()); n != 0 {
		t.Fatalf("a failed turn left %d proposals behind", n)
	}
}

// --- what became of earlier proposals ------------------------------------------------

// The model is told what became of what it proposed, in words it can repeat --
// otherwise it goes on calling an approved task "waiting".
func TestEarlierProposalsAreReportedWithTheirOutcome(t *testing.T) {
	h := newToolHarness(t, &ai.Mock{Reply: "Noted."})
	conv := h.conv.ID
	failed := "The task no longer exists, so it could not be changed."
	for _, a := range []actions.Action{
		{Status: actions.StatusExecuted, Input: json.RawMessage(`{"title":"Buy milk"}`)},
		{Status: actions.StatusRejected, Input: json.RawMessage(`{"title":"Buy eggs"}`)},
		{Status: actions.StatusFailed, Input: json.RawMessage(`{"title":"Buy bread"}`), ErrorMessage: &failed},
	} {
		a.ID, a.UserID, a.ConversationID, a.ToolName, a.Permission = uuid.New(), h.user, &conv, tools.CreateTask, tools.Write
		h.log.before = append(h.log.before, a)
	}
	// Somebody else's proposal in the same conversation id is not theirs to hear about.
	h.log.before = append(h.log.before, actions.Action{
		ID: uuid.New(), UserID: uuid.New(), ConversationID: &conv, ToolName: tools.CreateTask,
		Permission: tools.Write, Status: actions.StatusProposed, Input: json.RawMessage(`{"title":"Secret"}`),
	})

	if _, _, err := h.send(t, "thanks"); err != nil {
		t.Fatal(err)
	}
	prompt := h.answerPrompt()
	for _, want := range []string{
		`Create a task "Buy milk": the user approved it and it was done.`,
		`Create a task "Buy eggs": the user rejected it, so it was not done.`,
		`Create a task "Buy bread": the user approved it, but it failed: ` + failed,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "Secret") {
		t.Fatal("another user's proposal reached the prompt")
	}
}

// --- without tools --------------------------------------------------------------------

// An assistant with the Phase 7 dependencies unwired is the Phase 6 assistant:
// the read-only rule, no ACTIONS section, nothing recorded.
func TestWithoutToolsTheAssistantSaysItCannotAct(t *testing.T) {
	h := newHarness(t, &ai.Mock{Reply: "Taking actions is not supported yet."})
	turn, sink, err := h.send(t, "Create a task to buy milk tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	system := h.provider.LastPrompt()[0].Content
	if !strings.Contains(system, "taking actions is not supported yet") || strings.Contains(system, "ACTIONS section") {
		t.Fatalf("the system prompt is not the read-only one:\n%s", system)
	}
	if len(turn.Actions) != 0 || len(sink.Announced) != 0 || len(h.store.recordedActions()) != 0 {
		t.Fatal("an assistant without tools recorded an action")
	}
	if len(h.provider.Calls()) != 1 {
		t.Fatalf("an assistant without tools made %d model calls, want 1", len(h.provider.Calls()))
	}
}

// Wiring only some of the three is wiring none of them.
func TestToolsAreAllOrNothing(t *testing.T) {
	reg, _ := tools.NewRegistry(nil, tools.Standard(tools.Services{})...)
	for name, d := range map[string]Deps{
		"no router":  {Tools: reg, Actions: &actionLog{}},
		"no tools":   {Router: agents.NewRouter(&ai.Mock{}, reg, nil, agents.Options{}), Actions: &actionLog{}},
		"no actions": {Router: agents.NewRouter(&ai.Mock{}, reg, nil, agents.Options{}), Tools: reg},
	} {
		if NewService(d).toolsEnabled() {
			t.Fatalf("%s: tools enabled", name)
		}
	}
}
