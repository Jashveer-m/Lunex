// Package agents decides, for one user message, whether the assistant should
// use a tool and which one.
//
// An Agent here is data, not a type hierarchy: a name, a purpose and the set of
// tools it may use. Agents differ only in what they are allowed to do. How they
// decide (Router), how a decision is checked (the agent's own tool set, then
// the registry's validation) and what happens to it (a read runs, a write is
// proposed) is identical for every one of them -- so there is one Router, and
// an agent is the argument it is called with.
//
// There is exactly one agent the orchestrator uses, General, and it is
// composed out of six domain agents -- TaskAgent, GoalAgent, NoteAgent,
// DocumentAgent, CalendarAgent and FinanceAgent -- that each wrap one module's
// tools. They are not personas and nothing routes to them individually yet. They exist so that the named-agent
// layer the spec describes (a Study agent, a Career agent) is an addition
// rather than a rework: a later phase puts a first step in front of Decide that
// picks an agent, composes it from the domain agents and whatever new modules
// it needs, and calls the same Decide with it. Nothing about the tools, the
// action engine or the orchestrator changes. See docs/decisions.md.
package agents

import (
	"slices"

	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Agent is a named set of tools.
type Agent struct {
	Name string
	// Purpose says what the agent is for, in a phrase. It is not in this
	// phase's routing prompt -- there is one agent, and nothing chooses between
	// them -- but it is what a later agent-selection step would be given.
	Purpose string
	Tools   []string
}

// Allows reports whether the agent may use a tool. It is checked on the model's
// decision, not only when the prompt is built: a model that names a tool it was
// not offered gets no tool, rather than whichever one it named.
func (a Agent) Allows(tool string) bool { return slices.Contains(a.Tools, tool) }

// Compose is an agent that may use everything the given agents may, in order,
// each tool once.
func Compose(name, purpose string, agents ...Agent) Agent {
	out := Agent{Name: name, Purpose: purpose}
	for _, a := range agents {
		for _, t := range a.Tools {
			if !out.Allows(t) {
				out.Tools = append(out.Tools, t)
			}
		}
	}
	return out
}

// The domain agents. Each wraps one module's tools and nothing else.
var (
	TaskAgent = Agent{
		Name: "tasks", Purpose: "finding, creating and updating the user's tasks",
		Tools: []string{tools.SearchTasks, tools.CreateTask, tools.UpdateTask},
	}
	GoalAgent = Agent{
		Name: "goals", Purpose: "finding and creating the user's goals",
		Tools: []string{tools.SearchGoals, tools.CreateGoal},
	}
	NoteAgent = Agent{
		Name: "notes", Purpose: "finding and writing the user's notes",
		Tools: []string{tools.SearchNotes, tools.CreateNote},
	}
	DocumentAgent = Agent{
		Name: "documents", Purpose: "searching the user's uploaded documents",
		Tools: []string{tools.SearchDocuments},
	}
	CalendarAgent = Agent{
		Name: "calendar", Purpose: "looking at the user's calendar and scheduling events on it",
		Tools: []string{tools.SearchCalendar, tools.CreateCalendarEvent},
	}
	// FinanceAgent is Phase 9. Its purpose says "what they spent" and not
	// "their finances", and that wording is the point: this agent reads back
	// the user's own records and adds them up. It does not advise, and the
	// chat system prompt says so in the rule that goes with it.
	FinanceAgent = Agent{
		Name: "finance", Purpose: "looking at what the user spent, adding it up, and recording new expenses",
		Tools: []string{tools.SearchExpenses, tools.AnalyzeSpending, tools.CreateExpense},
	}
)

// General is the one agent the orchestrator uses: all of the domain agents'
// tools.
var General = Compose("general", "anything the assistant can do with the user's data",
	TaskAgent, GoalAgent, NoteAgent, DocumentAgent, CalendarAgent, FinanceAgent)
