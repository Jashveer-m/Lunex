package agents

import (
	"strings"
	"unicode"
)

// MightUseTool reports whether a message is worth a routing call at all.
//
// This is the routing counterpart of memories.WorthExtracting, and it exists
// for a measured cost. The routing call sits in front of the first token of
// every answer, and its prompt is ~720 tokens: on the development machine
// (an i5-7360U running Ollama on two threads) that is ~55 seconds of prompt
// evaluation whenever Ollama's prefix cache does not already hold it -- which,
// with chat and extraction prompts going through the same model between turns,
// is most of the time. Most messages are greetings, thanks, questions about
// the world and remarks about the user's day, and a request to use a tool
// names what it wants done to what: a task, a note, a goal, a document; add,
// find, mark, save.
//
// So the gate is lexical and deliberately permissive. It looks for one of the
// cue words below at the start of a word, and when it finds one the model
// decides; when it finds none, no tool is used and no call is made. It can
// only ever fail in one direction -- a request phrased with none of the cues
// gets an answer without a tool, the same answer Phase 6 would have given --
// and never the dangerous one: the gate can decline a tool, but only the model
// can choose one, and only the user can approve a write.
//
// A false positive costs one routing call and nothing else, which is why the
// list errs long: "which", "list" and "show" are in ordinary questions too.
func MightUseTool(message string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(message), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '-'
	}) {
		for _, cue := range toolCues {
			if strings.HasPrefix(w, cue) {
				return true
			}
		}
	}
	return false
}

// toolCues are word prefixes: "task" covers "tasks", "remind" covers
// "reminder", "complet" covers "complete", "completed" and "completion".
var toolCues = []string{
	// the things the tools act on
	"task", "todo", "to-do", "goal", "note", "document", "doc", "file", "pdf", "upload",
	"deadline", "due", "priorit", "milestone",
	// asking for something to exist
	"add", "create", "make", "new", "save", "jot", "write", "record", "log", "remind",
	"remember", "track", "put", "schedule", "set",
	// asking for something to change
	"mark", "change", "update", "edit", "rename", "move", "finish", "complet", "done",
	"postpone", "reschedule", "bump",
	// asking to find something
	"find", "search", "look", "show", "list", "which", "mention", "open", "pending",
	// asking to remove something: no tool can, and the router says so --
	// but the answer should come from a decision, not from the gate
	"delete", "remove", "cancel",
}
