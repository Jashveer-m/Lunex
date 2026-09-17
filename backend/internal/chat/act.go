package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// The three Phase 7 dependencies. They are wired together or not at all -- see
// Deps -- and each is the narrowest slice of its package the orchestrator
// needs.
type (
	// ToolRouter decides whether a message needs one of an agent's tools.
	ToolRouter interface {
		Decide(ctx context.Context, agent agents.Agent, message string) (agents.Decision, error)
	}
	// ToolRunner is the tool registry as the orchestrator sees it: it can turn
	// a model's call into a validated one, and it can run a *read*. There is no
	// method here that runs a write, and the registry has none that takes a
	// tool and an input -- so the chat turn could not execute a proposal even
	// if it tried. The only way a write runs is POST /actions/{id}/approve.
	ToolRunner interface {
		Prepare(ctx context.Context, userID uuid.UUID, name string, args tools.Args) (tools.Call, error)
		RunRead(ctx context.Context, userID uuid.UUID, call tools.Call) (tools.Result, error)
	}
	// ActionLog is the action engine's read side: what is already pending, and
	// what became of what was proposed earlier in this conversation. Recording
	// the turn's own actions is not here -- that happens in AppendTurn, in the
	// same transaction as the messages.
	ActionLog interface {
		PendingDuplicate(ctx context.Context, userID, conversationID uuid.UUID, call tools.Call) (actions.Action, bool, error)
		Recent(ctx context.Context, userID, conversationID uuid.UUID, limit int) ([]actions.Action, error)
		Describe(a actions.Action) string
	}
)

// MaxRecentActions is how many of the conversation's earlier proposals the
// model is reminded of, newest first. It is what stops the assistant saying
// "that is waiting for your approval" about a task the user approved an hour
// ago -- and five is more than any conversation with a person has open.
const MaxRecentActions = 5

// TurnAction is an action the turn produced, with the one-sentence summary a
// client shows for it.
type TurnAction struct {
	actions.Action
	Summary string
}

// toolStep is what the tool half of a turn decided and did, before anything is
// persisted.
type toolStep struct {
	// call is the validated tool call, or nil if no tool was used.
	call *tools.Call
	// result is a read's result.
	result tools.Result
	// sources is what a read retrieved, unlabelled; retrieve puts them first.
	sources []Source
	// reused is an identical proposal already waiting in this conversation,
	// surfaced again instead of being queued twice.
	reused *actions.Action
	// declined is why the model's call could not be prepared, in words for
	// the answering model.
	declined string
	// declinedTool is the tool whose call was declined.
	declinedTool string
	// recent is the conversation's earlier writes, for the prompt.
	recent []TurnAction
}

// newActions is what the turn records alongside its messages: a read that ran,
// or a write proposed for the first time. A reused proposal is already a row.
func (t toolStep) newActions() []actions.NewAction {
	switch {
	case t.call == nil || t.reused != nil:
		return nil
	case t.call.Permission == tools.Read:
		return []actions.NewAction{actions.Ran(*t.call, t.result)}
	default:
		return []actions.NewAction{actions.Proposal(*t.call)}
	}
}

// act is the tool half of a turn: decide, validate, and -- for a read -- run.
//
// It never writes anything. A write tool's call is validated into a proposal
// here and recorded by AppendTurn with the rest of the turn; running it is the
// action engine's business, on the user's approval, in a different request.
//
// Like retrieval, it happens before the sink is touched, so everything that
// can fail with a status code does. The router not answering is a 503; a read
// tool failing because the database or the embedder is down fails the turn
// the way a failed retrieval does -- answering "I found no tasks" when the
// search errored is a false statement about the user's data. A call the model
// got *wrong* is not a failure: it is shown to the answering model as a
// problem to tell the user about.
func (s *Service) act(ctx context.Context, userID, convID uuid.UUID, message string) (toolStep, error) {
	var step toolStep
	if !s.toolsEnabled() {
		return step, nil
	}

	recent, err := s.actions.Recent(ctx, userID, convID, MaxRecentActions)
	if err != nil {
		return step, fmt.Errorf("load recent actions: %w", err)
	}
	for _, a := range recent {
		step.recent = append(step.recent, TurnAction{Action: a, Summary: s.actions.Describe(a)})
	}

	if !agents.MightUseTool(message) {
		return step, nil
	}
	decision, err := s.router.Decide(ctx, s.agent, message)
	if err != nil {
		return step, err
	}
	if decision.None() {
		return step, nil
	}

	call, err := s.tools.Prepare(ctx, userID, decision.Tool, decision.Args)
	var argErr *tools.ArgumentError
	switch {
	case errors.As(err, &argErr):
		step.declined, step.declinedTool = argErr.Reason, argErr.Tool
		s.log.Info("tool call declined", "user_id", userID, "conversation_id", convID,
			"tool", argErr.Tool, "reason", argErr.Reason)
		return step, nil
	case errors.Is(err, tools.ErrUnknownTool):
		// The router only returns tools its agent offers, and the agent's are
		// the registry's; a miss here is a wiring bug, logged and survived.
		s.log.Error("routing returned a tool the registry does not have", "tool", decision.Tool)
		return step, nil
	case err != nil:
		return step, fmt.Errorf("prepare %s: %w", decision.Tool, err)
	}
	step.call = &call

	if call.Permission == tools.Write {
		dup, found, err := s.actions.PendingDuplicate(ctx, userID, convID, call)
		if err != nil {
			return step, fmt.Errorf("check for a pending duplicate: %w", err)
		}
		if found {
			step.reused = &dup
		}
		return step, nil
	}

	result, err := s.tools.RunRead(ctx, userID, call)
	if err != nil {
		return step, fmt.Errorf("run %s: %w", call.Tool, err)
	}
	step.result = result
	step.sources = toolSources(call.Tool, result)
	return step, nil
}

// toolsEnabled reports whether the Phase 7 dependencies are wired. They are
// all-or-nothing: a router with no registry to validate against, or a registry
// with nowhere to record a proposal, is not a partial feature but a broken one.
func (s *Service) toolsEnabled() bool {
	return s.router != nil && s.tools != nil && s.actions != nil
}

// toolSources renders a read's records as sources, marked with the tool that
// found them. They are rendered with the same summaries the heuristic uses, so
// a task a search found reads to the model exactly like one retrieved any
// other way -- what is different is that it was asked for.
func toolSources(tool string, r tools.Result) []Source {
	out := make([]Source, 0, r.Count())
	for _, c := range r.Chunks {
		idx, sim := c.ChunkIndex, c.Similarity
		out = append(out, Source{
			Type: SourceDocument, ID: c.DocumentID, Title: c.Filename, Tool: tool,
			ChunkIndex: &idx, Similarity: &sim, Excerpt: truncate(c.Content, MaxExcerptChars),
		})
	}
	for _, t := range r.Tasks {
		out = append(out, Source{Type: SourceTask, ID: t.ID, Title: t.Title, Tool: tool,
			Excerpt: truncate(taskSummary(t), MaxExcerptChars)})
	}
	for _, g := range r.Goals {
		out = append(out, Source{Type: SourceGoal, ID: g.ID, Title: g.Title, Tool: tool,
			Excerpt: truncate(goalSummary(g), MaxExcerptChars)})
	}
	for _, n := range r.Notes {
		out = append(out, Source{Type: SourceNote, ID: n.ID, Title: n.Title, Tool: tool,
			Excerpt: truncate(noteSummary(n), MaxExcerptChars)})
	}
	for _, e := range r.Events {
		out = append(out, Source{Type: SourceEvent, ID: e.ID, Title: e.Title, Tool: tool,
			Excerpt: truncate(eventSummary(e), MaxExcerptChars)})
	}
	return out
}

// actionsBlock renders what the tool step did, and what became of earlier
// proposals, for the model. It is empty when there is nothing to say.
//
// Each line states its status in words the model can repeat to the user
// without adding to them. The one it must never get wrong -- a proposal is not
// a change that happened -- is said in the line itself as well as in the
// system prompt, because the line is what the model reads last.
func (t toolStep) actionsBlock(sources []Source) string {
	var lines []string
	switch {
	case t.declined != "":
		lines = append(lines, fmt.Sprintf(
			"You tried to use the %s tool for this message and could not: %s. Nothing was proposed and nothing was changed.\n"+
				"In your reply, tell the user that in your own words and ask for what is missing.",
			t.declinedTool, t.declined))
	case t.call != nil && t.call.Permission == tools.Read:
		var labels []string
		for _, s := range sources {
			if s.Tool == t.call.Tool {
				labels = append(labels, "["+s.Label+"]")
			}
		}
		line := "You ran " + t.call.Tool + " (" + strings.TrimSuffix(t.call.Summary, ".") + "): "
		switch {
		case len(labels) == 0:
			line += "it found nothing.\nIn your reply, tell the user that nothing matched."
		case t.result.More:
			line += fmt.Sprintf("it found more than %d; the first %d are shown as %s.", len(labels), len(labels), strings.Join(labels, ", ")) +
				"\nIn your reply, answer the user's question from them in your own words and cite their labels."
		default:
			line += fmt.Sprintf("it found %d, shown as %s.", len(labels), strings.Join(labels, ", ")) +
				"\nIn your reply, answer the user's question from them in your own words and cite their labels."
		}
		lines = append(lines, line)
	case t.call != nil:
		// Worded as what to say rather than as a report, because that is what
		// was measured to work: llama3.2:3b shown "You proposed this change,
		// and it has NOT been made: ..." copied the section into its reply, and
		// shown this it answered "I have prepared a change ... it will only
		// happen once you approve it" in every sample.
		line := "You prepared a change that has not been made yet. Reply by telling the user, in your own words, " +
			"that you have prepared it, what it will do (" + strings.TrimSuffix(t.call.Summary, ".") + "), " +
			"and that it will only happen once they press Approve on the card shown with your reply (or Reject to cancel it). " +
			"Never say that it is done, and never ask the user to type or reply anything to approve it: typing in the chat does nothing."
		if t.reused != nil {
			line += " (The same change was already proposed earlier in this conversation and is still waiting; it was not proposed twice.)"
		}
		lines = append(lines, line)
	}

	var earlier []string
	for _, a := range t.recent {
		if t.reused != nil && a.ID == t.reused.ID {
			continue
		}
		earlier = append(earlier, "- "+strings.TrimSuffix(a.Summary, ".")+": "+statusInWords(a.Action))
	}
	if len(earlier) > 0 {
		lines = append(lines, "Earlier in this conversation you proposed:\n"+strings.Join(earlier, "\n"))
	}
	if len(lines) == 0 {
		return ""
	}
	return "ACTIONS\n\n" + strings.Join(lines, "\n\n")
}

// statusInWords is an action's status as a sentence the model can relay.
func statusInWords(a actions.Action) string {
	switch a.Status {
	case actions.StatusProposed:
		return "still waiting for the user to approve or reject it with the Approve or Reject button on its card."
	case actions.StatusApproved:
		return "the user approved it, and whether it completed is not known."
	case actions.StatusRejected:
		return "the user rejected it, so it was not done."
	case actions.StatusExecuted:
		return "the user approved it and it was done."
	case actions.StatusFailed:
		reason := "it failed."
		if a.ErrorMessage != nil {
			reason = "it failed: " + *a.ErrorMessage
		}
		return "the user approved it, but " + reason
	}
	return a.Status + "."
}

// turnActions pairs what AppendTurn recorded -- and a proposal reused from
// earlier -- with the summaries the client is shown. A reused proposal has the
// same canonical input as the call, so the call's summary describes it.
func (t toolStep) turnActions(recorded []actions.Action) []TurnAction {
	if t.call == nil {
		return nil
	}
	var out []TurnAction
	for _, a := range recorded {
		out = append(out, TurnAction{Action: a, Summary: t.call.Summary})
	}
	if t.reused != nil {
		out = append(out, TurnAction{Action: *t.reused, Summary: t.call.Summary})
	}
	return out
}

// unconfirmed is the text of every change this conversation has asked for that
// has not been made: the one this turn proposed (or found already waiting),
// and each earlier proposal that is still waiting, was rejected, or failed.
// An executed one is not in it -- that change is reality. It is what the
// extractors are told not to read as fact; see memories.Turn.
func (t toolStep) unconfirmed() []string {
	var out []string
	if t.call != nil && t.call.Permission == tools.Write {
		out = append(out, changeText(t.call.Input))
	}
	for _, a := range t.recent {
		if a.Status != actions.StatusExecuted {
			out = append(out, changeText(a.Input))
		}
	}
	return out
}

// changeText is what a change is about, read off its canonical input: the
// title, content and the rest of the free text it carries -- not the ids and
// dates, and not the closed-set values (a status of "completed", a priority of
// "high"), which say how the change is made rather than what it is about and
// would otherwise match any fact that uses the word.
//
// An event's `start` and `end` are dates by another name, and are skipped for
// the same reason `deadline` is.
func changeText(input []byte) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) != nil {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch k {
		case "deadline", "start", "end", "status", "priority", "type":
			continue
		}
		if strings.HasSuffix(k, "_id") {
			continue
		}
		switch v := fields[k].(type) {
		case string:
			parts = append(parts, v)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					parts = append(parts, s)
				}
			}
		}
	}
	return strings.Join(parts, "; ")
}

// toolName is the tool the turn used, for the log.
func (t toolStep) toolName() string {
	switch {
	case t.call != nil:
		return t.call.Tool
	case t.declinedTool != "":
		return t.declinedTool + " (declined)"
	}
	return ""
}
