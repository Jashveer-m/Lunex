package agents

import (
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// MaxMessageChars truncates the message the router reads. The decision is
// about what the user is asking for, and that is in the first two thousand
// characters of anything a person types into a chat box.
const MaxMessageChars = 2_000

// NoTool is the decision the prompt offers for "no tool is needed". It is a
// named choice rather than an empty reply on purpose, and that is measured: a
// prompt asking for a list of calls, where "no tool" is an empty list, got `[]`
// from llama3.2:3b on every message -- including ones that plainly asked for a
// task to be created. See docs/decisions.md.
const NoTool = "none"

// routingSystemPrompt is the instruction half of the routing call. The tool
// list is rendered into it from the registry, so what the model is offered is
// exactly what the registry will accept.
//
// It is a classifier's prompt, not a conversation's: it does not see the
// retrieved context, the conversation history or the date, and it never
// answers the message. That is what keeps it short -- the call is on the path
// to the first token, and every token of prompt is prompt evaluation the user
// waits for -- and it is why a relative date is copied rather than resolved:
// tools.ParseDate does the arithmetic.
const routingSystemPrompt = `You are the tool selector for a personal assistant. You read one message from the user and decide which tool, if any, the assistant should use for it. You never answer the message yourself.

Tools:
%TOOLS%
- none: no tool is needed. Use this for greetings, thanks, general knowledge questions, and things the user tells you about their day.

Nothing can be deleted: a request to delete something is "none".
A reply like "yes" or "ok" is "none".

Only give an argument the message states. Leave out every optional argument the message does not give -- never guess one and never write a placeholder such as "unknown".

Reply with one JSON object and nothing else: {"tool": "<tool name>", "arguments": {<only the arguments the message gives>}}`

// shot is one worked example, replayed as a real user/assistant exchange.
//
// Examples as turns rather than as text inside the system prompt is the other
// measured half of this prompt: the same examples written inline took the
// model to `[]` for everything, and as turns they took it to 18 out of 18.
type shot struct {
	tool    string // the tool the example demonstrates, or NoTool
	message string
	reply   string
}

// shots are shown only when the agent may use the tool they demonstrate, so a
// narrower agent is never taught to call something it cannot. The NoTool
// examples are always shown: every agent can decline.
var shots = []shot{
	{tools.CreateTask, "Add a task to renew my passport by 2026-10-01, it's urgent.",
		`{"tool": "create_task", "arguments": {"title": "Renew my passport", "deadline": "2026-10-01", "priority": "high"}}`},
	{NoTool, "What is a good way to learn Rust?", `{"tool": "none", "arguments": {}}`},
	{tools.SearchNotes, "Which of my notes mention the generator?",
		`{"tool": "search_notes", "arguments": {"query": "generator"}}`},
	{NoTool, "Good morning!", `{"tool": "none", "arguments": {}}`},
	{tools.UpdateTask, "I've finished the essay task, mark it complete",
		`{"tool": "update_task", "arguments": {"task": "essay", "status": "completed"}}`},
	// The two below were added after the first run of the eval in
	// ollama_eval_test.go: without them "remind me to ..." got no tool, and a
	// statement containing "finish" got a search. They are different instances
	// of those patterns from the eval's own, so the eval still measures
	// something.
	{tools.CreateTask, "Remind me to water the plants on Sunday",
		`{"tool": "create_task", "arguments": {"title": "Water the plants", "deadline": "sunday"}}`},
	{NoTool, "I've been really busy finishing my thesis chapters this week.", `{"tool": "none", "arguments": {}}`},
	// The calendar pair. The first teaches that a question about the diary is a
	// look rather than a search of anything else; the second that the time of
	// day is copied as the user wrote it, like every other date in this prompt.
	{tools.SearchCalendar, "What's on my calendar tomorrow?",
		`{"tool": "search_calendar", "arguments": {"start": "tomorrow"}}`},
	{tools.CreateCalendarEvent, "Book the dentist for Thursday at 3pm",
		`{"tool": "create_calendar_event", "arguments": {"title": "Dentist", "start": "thursday at 3pm"}}`},
}

// RoutingPrompt is the prompt for one routing decision: the system
// instructions with the offered tools rendered in, the worked examples, and the
// message.
func RoutingPrompt(offered []tools.Tool, message string) []ai.Message {
	allowed := make(map[string]bool, len(offered))
	for _, t := range offered {
		allowed[t.Name] = true
	}
	out := []ai.Message{{
		Role:    ai.RoleSystem,
		Content: strings.Replace(routingSystemPrompt, "%TOOLS%", renderTools(offered), 1),
	}}
	for _, s := range shots {
		if s.tool != NoTool && !allowed[s.tool] {
			continue
		}
		out = append(out,
			ai.Message{Role: ai.RoleUser, Content: s.message},
			ai.Message{Role: ai.RoleAssistant, Content: s.reply})
	}
	return append(out, ai.Message{Role: ai.RoleUser, Content: truncate(strings.TrimSpace(message), MaxMessageChars)})
}

// renderTools writes each tool as a line with its parameters indented under it.
// The format is plain text rather than JSON Schema: it is what the prompt was
// measured with, and a 3B model reads a short list more reliably than a schema
// document.
func renderTools(offered []tools.Tool) string {
	var b strings.Builder
	for i, t := range offered {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("- " + t.Name + ": " + t.Description)
		for _, p := range t.Params {
			b.WriteString("\n    " + p.Name)
			if p.Required {
				b.WriteString(" (required)")
			}
			b.WriteString(": " + p.Description)
			if len(p.Enum) > 0 {
				b.WriteString(" (" + orList(p.Enum) + ")")
			}
		}
	}
	return b.String()
}

// orList renders ["a", "b", "c"] as "a, b or c".
func orList(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}
