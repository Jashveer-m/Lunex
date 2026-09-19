package agents

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// catalog is the standard tool declarations. Nothing here runs a tool, so the
// services behind them are never reached.
type catalog []tools.Tool

func (c catalog) Tools() []tools.Tool { return c }

func standard() catalog { return catalog(tools.Standard(tools.Services{})) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- agents -----------------------------------------------------------------

// General is exactly the domain agents' tools, and every standard tool belongs
// to exactly one domain agent -- which is what makes the domain agents a real
// partition a named-agent layer can compose, rather than labels.
func TestGeneralIsTheUnionOfTheDomainAgents(t *testing.T) {
	owner := map[string]string{}
	for _, a := range []Agent{TaskAgent, GoalAgent, NoteAgent, DocumentAgent, CalendarAgent,
		FinanceAgent, StudyAgent} {
		for _, name := range a.Tools {
			if prev, dup := owner[name]; dup {
				t.Fatalf("%s belongs to both %s and %s", name, prev, a.Name)
			}
			owner[name] = a.Name
		}
	}
	for _, tool := range standard() {
		if owner[tool.Name] == "" {
			t.Fatalf("%s belongs to no domain agent", tool.Name)
		}
		if !General.Allows(tool.Name) {
			t.Fatalf("General cannot use %s", tool.Name)
		}
	}
	if len(General.Tools) != len(standard()) {
		t.Fatalf("General has %d tools, want %d", len(General.Tools), len(standard()))
	}
}

func TestComposeKeepsEachToolOnce(t *testing.T) {
	a := Compose("both", "", TaskAgent, TaskAgent, NoteAgent)
	if len(a.Tools) != len(TaskAgent.Tools)+len(NoteAgent.Tools) {
		t.Fatalf("tools = %v", a.Tools)
	}
}

// --- the prompt -------------------------------------------------------------

func TestTheRoutingPromptOffersExactlyTheAgentsTools(t *testing.T) {
	var offered []tools.Tool
	for _, tool := range standard() {
		if TaskAgent.Allows(tool.Name) {
			offered = append(offered, tool)
		}
	}
	prompt := RoutingPrompt(offered, "  Add a task to buy milk  ")
	system := prompt[0].Content
	for _, want := range []string{"- search_tasks:", "- create_task:", "- update_task:", "- none:",
		"title (required)", "(low, medium or high)", "Nothing can be deleted"} {
		if !strings.Contains(system, want) {
			t.Fatalf("the prompt is missing %q:\n%s", want, system)
		}
	}
	for _, not := range []string{"create_note", "search_documents", "%TOOLS%"} {
		if strings.Contains(system, not) {
			t.Fatalf("the prompt offers %q to the task agent", not)
		}
	}
	// The worked examples are real turns, and none demonstrates a tool this
	// agent cannot use: the search_notes example is gone, the none ones stay.
	all := ai.PromptText(prompt)
	if strings.Contains(all, "search_notes") {
		t.Fatal("the task agent was shown an example of a tool it may not use")
	}
	if !strings.Contains(all, `"tool": "none"`) || !strings.Contains(all, `"tool": "create_task"`) {
		t.Fatal("the examples are missing")
	}
	last := prompt[len(prompt)-1]
	if last.Role != ai.RoleUser || last.Content != "Add a task to buy milk" {
		t.Fatalf("the last message is %+v, want the user's message", last)
	}
}

func TestTheRoutingPromptTruncatesAVeryLongMessage(t *testing.T) {
	prompt := RoutingPrompt(standard(), strings.Repeat("x", MaxMessageChars*2))
	if n := len([]rune(prompt[len(prompt)-1].Content)); n > MaxMessageChars+1 {
		t.Fatalf("message is %d runes", n)
	}
}

// --- parsing ----------------------------------------------------------------

// Every shape here is one a model has been seen to write, or plausibly would.
func TestParseDecisionAcceptsTheShapesModelsWrite(t *testing.T) {
	for _, tc := range []struct {
		name, reply, tool, arg, want string
	}{
		{"as asked", `{"tool": "create_task", "arguments": {"title": "Buy milk"}}`, "create_task", "title", "Buy milk"},
		// Seen from llama3.2:3b during the routing measurement.
		{"flattened", `{"tool": "search_documents", "query": "aurora"}`, "search_documents", "query", "aurora"},
		{"native habit", `{"name": "search_notes", "parameters": {"query": "Rust"}}`, "search_notes", "query", "Rust"},
		{"openai", `{"function": {"name": "create_note", "arguments": "{\"title\": \"wifi\"}"}}`, "create_note", "title", "wifi"},
		{"a list of one", `[{"tool": "update_task", "arguments": {"task": "essay"}}]`, "update_task", "task", "essay"},
		{"fenced", "```json\n{\"tool\": \"search_tasks\", \"arguments\": {\"query\": \"x\"}}\n```", "search_tasks", "query", "x"},
		{"after prose", `Sure! Here you go: {"tool": "Create-Task", "args": {"title": "t"}}`, "create_task", "title", "t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ParseDecision(tc.reply)
			if d.Tool != tc.tool || d.Args.String(tc.arg) != tc.want {
				t.Fatalf("ParseDecision = %+v, want %s with %s=%s", d, tc.tool, tc.arg, tc.want)
			}
		})
	}
}

func TestParseDecisionInventsNothing(t *testing.T) {
	for _, reply := range []string{
		`{"tool": "none", "arguments": {}}`,
		`{"tool": "", "arguments": {"title": "x"}}`,
		`{"arguments": {"title": "x"}}`,
		`[]`,
		`I think you should create a task for that.`,
		`{"tool": "create_task", "arguments": `,
		``,
	} {
		if d := ParseDecision(reply); !d.None() {
			t.Fatalf("ParseDecision(%q) = %+v, want no tool", reply, d)
		}
	}
}

// --- the router -------------------------------------------------------------

func TestTheRouterReturnsTheModelsDecision(t *testing.T) {
	m := &ai.Mock{Reply: `{"tool": "create_task", "arguments": {"title": "Buy milk", "deadline": "tomorrow"}}`}
	r := NewRouter(m, standard(), quiet(), Options{Model: "router-model"})
	d, err := r.Decide(context.Background(), General, "Create a task to buy milk tomorrow")
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != tools.CreateTask || d.Args.String("deadline") != "tomorrow" {
		t.Fatalf("decision = %+v", d)
	}
	opts := m.Calls()[0].Options
	if opts.Model != "router-model" || opts.Temperature != DefaultTemperature || opts.MaxTokens != DefaultMaxTokens {
		t.Fatalf("options = %+v", opts)
	}
	// Measured: JSON mode is not what this prompt was measured with.
	if opts.Format != "" {
		t.Fatalf("the routing call asked for format %q", opts.Format)
	}
}

// A model naming a tool it was not offered -- or one that does not exist --
// gets no tool, not the one it named.
func TestTheRouterOnlyReturnsToolsTheAgentMayUse(t *testing.T) {
	for name, tc := range map[string]struct {
		agent Agent
		reply string
	}{
		"outside the agent": {TaskAgent, `{"tool": "create_note", "arguments": {"title": "x"}}`},
		"not a tool":        {General, `{"tool": "delete_task", "arguments": {"task": "x"}}`},
		"shell out":         {General, `{"tool": "run_sql", "arguments": {"sql": "DROP TABLE tasks"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(&ai.Mock{Reply: tc.reply}, standard(), quiet(), Options{})
			d, err := r.Decide(context.Background(), tc.agent, "whatever")
			if err != nil || !d.None() {
				t.Fatalf("Decide = %+v, %v; want no tool", d, err)
			}
		})
	}
}

// The model not answering is an outage the turn reports as a 503, not a
// decision to use no tool: answering without the step that decides whether
// the user asked for something is not an answer to give silently.
func TestARoutingFailureIsTheModelBeingUnavailable(t *testing.T) {
	for name, m := range map[string]*ai.Mock{
		"refused":   {Err: errors.New("connection refused")},
		"truncated": {Reply: `{"tool": "create`, StreamErr: errors.New("stream ended")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewRouter(m, standard(), quiet(), Options{}).Decide(context.Background(), General, "add a task")
			if !errors.Is(err, ai.ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestTheRoutersOwnDeadlineIsAnOutageButTheCallersIsNot(t *testing.T) {
	r := NewRouter(&blockingProvider{}, standard(), quiet(), Options{Timeout: 20 * time.Millisecond})
	if _, err := r.Decide(context.Background(), General, "add a task"); !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("the router's deadline = %v, want ErrUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = NewRouter(&blockingProvider{}, standard(), quiet(), Options{Timeout: time.Minute})
	if _, err := r.Decide(ctx, General, "add a task"); !errors.Is(err, context.Canceled) || errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("a caller that hung up = %v, want context.Canceled and not an outage", err)
	}
}

// blockingProvider accepts the request and then waits for the context, the
// way a model that has not produced a token yet does.
type blockingProvider struct{}

func (blockingProvider) Model() string { return "blocking" }
func (blockingProvider) Chat(ctx context.Context, _ []ai.Message, _ ai.Options) (ai.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAnAgentWithNoToolsIsNeverRouted(t *testing.T) {
	m := &ai.Mock{Reply: `{"tool": "create_task"}`}
	d, err := NewRouter(m, standard(), quiet(), Options{}).Decide(context.Background(), Agent{Name: "empty"}, "add a task")
	if err != nil || !d.None() || len(m.Calls()) != 0 {
		t.Fatalf("Decide = %+v, %v after %d calls", d, err, len(m.Calls()))
	}
}

// --- the gate ---------------------------------------------------------------

// The gate passes every message of the routing measurement that needed a tool,
// and none of the conversational ones -- except a delete request, which the
// router then declines.
func TestTheGateSkipsConversationAndKeepsRequests(t *testing.T) {
	for _, m := range []string{
		"Create a task to buy milk tomorrow",
		"Add a high priority task: submit the tax return by 2026-09-30",
		"Mark the scheduler task as done",
		"Change the priority of the compiler project task to high",
		"Which of my tasks mention the antenna?",
		"Find my notes about Rust",
		"Show my active goals",
		"What do my uploaded documents say about the aurora?",
		"Set a goal to run a marathon next year",
		"Save a note: the wifi password for the cabin is hunter2",
		"Remind me to call the bank on Friday",
		"Delete my task about the antenna",
		"I finished the essay",
	} {
		if !MightUseTool(m) {
			t.Fatalf("the gate skipped %q", m)
		}
	}
	for _, m := range []string{
		"What is the capital of France?",
		"Hi, how are you?",
		"Thanks, that's really helpful",
		"I went for a run this morning and felt great.",
		"Explain how Go channels work",
		"yes, go ahead",
		"What should I focus on today?",
		"",
	} {
		if MightUseTool(m) {
			t.Fatalf("the gate would route %q", m)
		}
	}
}

// A filter value has to come from the message. Measured: llama3.2:3b routed
// questions about seedlings to search_notes with `tag: "<unknown>"` and with
// `tag: "greenhouse"` -- neither said by the user -- and the searches ran with
// them and found nothing. The router leaves an ungrounded filter out, keeps the
// rest of the call, and keeps a filter the user did give.
func TestTheRouterDropsFilterValuesTheMessageDoesNotGive(t *testing.T) {
	for name, tc := range map[string]struct {
		message, reply string
		tool, param    string
		want           string // "" means the argument must be gone
	}{
		"an invented tag": {
			"What do my notes say about watering the seedlings?",
			`{"tool": "search_notes", "arguments": {"query": "seedlings", "tag": "greenhouse"}}`,
			tools.SearchNotes, "tag", ""},
		"a placeholder tag": {
			"What do my notes say about watering the seedlings?",
			`{"tool": "search_notes", "arguments": {"query": "seedlings", "tag": "<unknown>"}}`,
			tools.SearchNotes, "tag", ""},
		"a topic word is not a tag": {
			"Search my greenhouse notes for seedlings",
			`{"tool": "search_notes", "arguments": {"query": "seedlings", "tag": "greenhouse"}}`,
			tools.SearchNotes, "tag", ""},
		"a tag the user named": {
			"Search my notes tagged greenhouse for seedlings",
			`{"tool": "search_notes", "arguments": {"query": "seedlings", "tag": "greenhouse"}}`,
			tools.SearchNotes, "tag", "greenhouse"},
		"a hashtag": {
			"anything in #greenhouse about seedlings?",
			`{"tool": "search_notes", "arguments": {"query": "seedlings", "tag": "greenhouse"}}`,
			tools.SearchNotes, "tag", "greenhouse"},
		"an invented status": {
			"Which of my tasks mention the antenna?",
			`{"tool": "search_tasks", "arguments": {"query": "antenna", "status": "pending"}}`,
			tools.SearchTasks, "status", ""},
		"a status named by a synonym": {
			"Which tasks have I finished this week?",
			`{"tool": "search_tasks", "arguments": {"status": "completed"}}`,
			tools.SearchTasks, "status", "completed"},
		"a goal status named as written": {
			"Show my active goals",
			`{"tool": "search_goals", "arguments": {"status": "active"}}`,
			tools.SearchGoals, "status", "active"},
		"an invented goal status": {
			"What goals do I have about running?",
			`{"tool": "search_goals", "arguments": {"query": "running", "status": "abandoned"}}`,
			tools.SearchGoals, "status", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(&ai.Mock{Reply: tc.reply}, standard(), quiet(), Options{})
			d, err := r.Decide(context.Background(), General, tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tool != tc.tool {
				t.Fatalf("decision = %+v, want %s kept", d, tc.tool)
			}
			_, present := d.Args[tc.param]
			switch {
			case tc.want == "" && present:
				t.Fatalf("%s = %v survived; the message does not give it", tc.param, d.Args[tc.param])
			case tc.want != "" && d.Args.String(tc.param) != tc.want:
				t.Fatalf("%s = %v, want %q kept", tc.param, d.Args[tc.param], tc.want)
			}
			// The rest of the call is kept: the question is still answered.
			if q, ok := d.Args["query"]; ok && q == "" {
				t.Fatalf("query lost: %+v", d.Args)
			}
		})
	}
}

// Filters are the only arguments checked. A create_task's title is the model's
// own wording and a priority is a judgement; both are shown to the user in the
// proposal before anything happens.
func TestTheRouterDoesNotGroundWriteArguments(t *testing.T) {
	r := NewRouter(&ai.Mock{Reply: `{"tool": "create_task", "arguments": {"title": "Renew passport", "priority": "high"}}`},
		standard(), quiet(), Options{})
	d, err := r.Decide(context.Background(), General, "Add a task to renew my passport, it's urgent")
	if err != nil {
		t.Fatal(err)
	}
	if d.Args.String("title") != "Renew passport" || d.Args.String("priority") != "high" {
		t.Fatalf("decision = %+v, want the write's arguments untouched", d)
	}
}

// The same rule for the calendar's window, which is the Phase 8 case of it: a
// date range the user did not give would not empty a search, it would answer a
// question about the wrong days -- and "nothing on Tuesday" is a sentence the
// user has no way to tell from the truth.
//
// A dropped window is not a failed call. The tool falls back to its stated
// default and the summary names the days it looked at, so the answer says which
// week it was about.
func TestTheRouterDropsCalendarDatesTheMessageDoesNotGive(t *testing.T) {
	for name, tc := range map[string]struct {
		message, reply string
		want           map[string]string // argument -> value, "" meaning it must be gone
	}{
		"dates nobody gave": {
			"What's on my calendar?",
			`{"tool": "search_calendar", "arguments": {"start": "2026-09-10", "end": "2026-09-17"}}`,
			map[string]string{"start": "", "end": ""}},
		"the day the user named": {
			"What's on my calendar tomorrow?",
			`{"tool": "search_calendar", "arguments": {"start": "tomorrow"}}`,
			map[string]string{"start": "tomorrow"}},
		"a stretch the user named": {
			"Am I busy next week?",
			`{"tool": "search_calendar", "arguments": {"start": "next week"}}`,
			map[string]string{"start": "next week"}},
		"a date written as the user wrote it": {
			"What have I got on between 2026-09-14 and 2026-09-18?",
			`{"tool": "search_calendar", "arguments": {"start": "2026-09-14", "end": "2026-09-18"}}`,
			map[string]string{"start": "2026-09-14", "end": "2026-09-18"}},
		// The aliases are declared on the parameter, so a window written under
		// one is checked exactly like one written under its own name --
		// otherwise "when" would be the way round the rule.
		"an invented window under an alias": {
			"What's on my calendar?",
			`{"tool": "search_calendar", "arguments": {"when": "next month"}}`,
			map[string]string{"when": ""}},
		"a window under an alias the user gave": {
			"What's on my calendar next month?",
			`{"tool": "search_calendar", "arguments": {"when": "next month"}}`,
			map[string]string{"when": "next month"}},
		"a placeholder": {
			"What's on my calendar?",
			`{"tool": "search_calendar", "arguments": {"start": "<unknown>"}}`,
			map[string]string{"start": ""}},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(&ai.Mock{Reply: tc.reply}, standard(), quiet(), Options{})
			d, err := r.Decide(context.Background(), General, tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tool != tools.SearchCalendar {
				t.Fatalf("decision = %+v, want the call kept", d)
			}
			for arg, want := range tc.want {
				_, present := d.Args[arg]
				switch {
				case want == "" && present:
					t.Fatalf("%s = %v survived; the message does not give it", arg, d.Args[arg])
				case want != "" && d.Args.String(arg) != want:
					t.Fatalf("%s = %v, want %q kept", arg, d.Args[arg], want)
				}
			}
		})
	}
}

// A proposed event's own details are not filtered: the time and the title are
// shown to the user in the proposal before anything is written, which is the
// check that applies to a write.
func TestTheRouterDoesNotGroundAProposedEvent(t *testing.T) {
	r := NewRouter(&ai.Mock{Reply: `{"tool": "create_calendar_event", "arguments": {"title": "Dentist", "start": "thursday at 3pm"}}`},
		standard(), quiet(), Options{})
	d, err := r.Decide(context.Background(), General, "Book the dentist for Thursday afternoon")
	if err != nil {
		t.Fatal(err)
	}
	if d.Args.String("title") != "Dentist" || d.Args.String("start") != "thursday at 3pm" {
		t.Fatalf("decision = %+v, want the write's arguments untouched", d)
	}
}

// The same rule for the finance filters, which is the Phase 9 case of it.
//
// A spending total is the sharpest version of the failure this rule exists for.
// An invented tag empties a search and the user is told nothing matched; an
// invented *period* produces a number -- a real total, over days nobody asked
// about -- and there is nothing in "you spent ₹12,400" for the user to tell
// apart from the truth. An invented category does the same thing one step
// further in: a total over the wrong subset of their own money.
func TestTheRouterDropsFinanceFiltersTheMessageDoesNotGive(t *testing.T) {
	for name, tc := range map[string]struct {
		message, reply string
		tool           string
		want           map[string]string // argument -> value, "" meaning it must be gone
	}{
		"a period nobody gave": {
			"How much have I been spending?",
			`{"tool": "analyze_spending", "arguments": {"start": "2026-08-01", "end": "2026-08-31"}}`,
			tools.AnalyzeSpending, map[string]string{"start": "", "end": ""}},
		"the month the user named": {
			"How much did I spend last month?",
			`{"tool": "analyze_spending", "arguments": {"start": "last month"}}`,
			tools.AnalyzeSpending, map[string]string{"start": "last month"}},
		"a category the user named": {
			"How much have I spent on food this month?",
			`{"tool": "analyze_spending", "arguments": {"category": "food", "start": "this month"}}`,
			tools.AnalyzeSpending, map[string]string{"category": "food", "start": "this month"}},
		"a category nobody named": {
			"How much have I spent this month?",
			`{"tool": "analyze_spending", "arguments": {"category": "food", "start": "this month"}}`,
			tools.AnalyzeSpending, map[string]string{"category": "", "start": "this month"}},
		// The aliases are declared on the parameter, so a filter written under
		// one is checked exactly like one written under its own name --
		// otherwise "period" would be the way round the rule.
		"an invented period under an alias": {
			"What have I been spending money on?",
			`{"tool": "analyze_spending", "arguments": {"period": "last 3 months"}}`,
			tools.AnalyzeSpending, map[string]string{"period": ""}},
		"a placeholder category": {
			"Show me what I've spent",
			`{"tool": "search_expenses", "arguments": {"category": "<unknown>"}}`,
			tools.SearchExpenses, map[string]string{"category": ""}},
		"dates on a list": {
			"List my expenses",
			`{"tool": "search_expenses", "arguments": {"start": "2026-09-01", "end": "2026-09-30"}}`,
			tools.SearchExpenses, map[string]string{"start": "", "end": ""}},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(&ai.Mock{Reply: tc.reply}, standard(), quiet(), Options{})
			d, err := r.Decide(context.Background(), General, tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tool != tc.tool {
				t.Fatalf("decision = %+v, want %s kept", d, tc.tool)
			}
			for arg, want := range tc.want {
				_, present := d.Args[arg]
				switch {
				case want == "" && present:
					t.Fatalf("%s = %v survived; the message does not give it", arg, d.Args[arg])
				case want != "" && d.Args.String(arg) != want:
					t.Fatalf("%s = %v, want %q kept", arg, d.Args[arg], want)
				}
			}
		})
	}
}

// A proposed expense's own details are not filtered: the amount and what it was
// for are shown to the user in the proposal before anything is written.
func TestTheRouterDoesNotGroundAProposedExpense(t *testing.T) {
	r := NewRouter(&ai.Mock{Reply: `{"tool": "create_expense", "arguments": {"amount": "450", "description": "lunch", "category": "Food"}}`},
		standard(), quiet(), Options{})
	d, err := r.Decide(context.Background(), General, "I spent 450 on lunch today")
	if err != nil {
		t.Fatal(err)
	}
	if d.Args.String("amount") != "450" || d.Args.String("category") != "Food" {
		t.Fatalf("decision = %+v, want the write's arguments untouched", d)
	}
}

// The gate lets a money message through to the router at all. It can only fail
// in one direction -- a request phrased with none of the cues gets an answer
// without a tool -- but a phase whose whole subject is money should not be shut
// out by the word list in front of it.
func TestTheGateLetsMoneyMessagesThrough(t *testing.T) {
	for _, m := range []string{
		"I spent 450 on lunch today",
		"How much did I spend on food last month?",
		"log a 1200 rupee expense for groceries",
		"what has my spending looked like this year?",
		"I paid the electricity bill yesterday",
		"can I afford a new laptop?",
		"should I put more money into savings?",
	} {
		if !MightUseTool(m) {
			t.Fatalf("the gate would not route %q", m)
		}
	}
}

// The one write argument that is grounded, and the measured reason it is.
//
// On "I spent 1450.50 on printer cartridges today, log it", llama3.2:3b wrote
// `currency: "USD"` for a message with no currency in it. The proposal then
// read "Record an expense of USD 1450.50", which looks to the user like
// something they said -- and an expense filed in a currency they never named is
// money that is never totalled with the rest of theirs, because nothing here
// converts between currencies.
//
// So the currency has to come from the message, in a code, a word or a symbol.
// Everything else about the proposal stays: the amount and the description are
// the user's own words echoed back, and they are checked by the user's eyes.
func TestTheRouterDropsACurrencyTheMessageDoesNotGive(t *testing.T) {
	for name, tc := range map[string]struct {
		message, reply string
		want           string // "" means the argument must be gone
	}{
		"a currency nobody gave": {
			"I spent 1450.50 on printer cartridges today, log it.",
			`{"tool": "create_expense", "arguments": {"amount": "1450.50", "currency": "USD", "description": "printer cartridges"}}`,
			""},
		"the code the user wrote": {
			"I spent 20 USD on a book",
			`{"tool": "create_expense", "arguments": {"amount": "20", "currency": "USD", "description": "book"}}`,
			"USD"},
		"a currency named in words": {
			"I spent 20 dollars on a book",
			`{"tool": "create_expense", "arguments": {"amount": "20", "currency": "USD", "description": "book"}}`,
			"USD"},
		// A symbol has no word boundary between it and the number, so the
		// ordinary whole-words test would never see it.
		"a currency named by its symbol": {
			"I spent $20 on a book",
			`{"tool": "create_expense", "arguments": {"amount": "20", "currency": "USD", "description": "book"}}`,
			"USD"},
		"rupees": {
			"I spent 450 rupees on lunch",
			`{"tool": "create_expense", "arguments": {"amount": "450", "currency": "INR", "description": "lunch"}}`,
			"INR"},
		// A currency the synonym table does not have is still kept when the
		// user writes the code itself.
		"a code the table does not know": {
			"I spent 30 CHF on lunch in Zurich",
			`{"tool": "create_expense", "arguments": {"amount": "30", "currency": "CHF", "description": "lunch"}}`,
			"CHF"},
		"an invented currency under an alias": {
			"I spent 1450.50 on printer cartridges",
			`{"tool": "create_expense", "arguments": {"amount": "1450.50", "curr": "USD"}}`,
			""},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewRouter(&ai.Mock{Reply: tc.reply}, standard(), quiet(), Options{})
			d, err := r.Decide(context.Background(), General, tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if d.Tool != tools.CreateExpense {
				t.Fatalf("decision = %+v, want the call kept", d)
			}
			got := d.Args.String("currency", "curr")
			if got != tc.want {
				t.Fatalf("currency = %q, want %q", got, tc.want)
			}
			// The rest of the proposal is untouched either way.
			if d.Args.String("amount") == "" {
				t.Fatalf("the amount was dropped: %+v", d.Args)
			}
		})
	}
}
