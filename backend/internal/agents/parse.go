package agents

import (
	"encoding/json"
	"strings"

	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Decision is what the router concluded: a tool and the arguments the model
// suggested for it, or no tool at all (Tool == "").
//
// The arguments are exactly as the model wrote them. Turning them into
// something that may run is the registry's job, and it is strict about it.
type Decision struct {
	Tool string
	Args tools.Args
}

// None reports whether the decision is "no tool".
func (d Decision) None() bool { return d.Tool == "" }

// ParseDecision reads a routing reply.
//
// It is lenient about shape in the way the extraction parsers are, and for the
// same reason -- every shape here is one llama3.2:3b has been seen to produce,
// or one a different model behind AGENT_MODEL plausibly would:
//
//	{"tool": "create_task", "arguments": {...}}            the shape asked for
//	{"tool": "search_documents", "query": "aurora"}        arguments flattened
//	{"name": "create_task", "parameters": {...}}           a native tool-call habit
//	{"function": {"name": ..., "arguments": "{...}"}}      OpenAI's, arguments as a string
//	[{"tool": ...}]                                        a list of one
//
// inside a Markdown fence or after a line of prose. It is strict about what it
// returns: no tool unless a name was found, and the name is only normalized for
// case and separators -- whether it is a tool at all, and one this agent may
// use, is the Router's check.
func ParseDecision(reply string) Decision {
	value := firstJSONValue(stripFence(strings.TrimSpace(reply)))
	if value == "" {
		return Decision{}
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil {
		var list []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &list); err != nil || len(list) == 0 {
			return Decision{}
		}
		obj = list[0]
	}

	// {"function": {"name": ..., "arguments": ...}} nests the call one level
	// down; everything below reads the inner object.
	if inner, ok := obj["function"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(inner, &nested) == nil {
			obj = nested
		}
	}

	nameKey := ""
	name := ""
	for _, k := range []string{"tool", "name", "tool_name", "action", "function"} {
		var s string
		if raw, ok := obj[k]; ok && json.Unmarshal(raw, &s) == nil {
			nameKey, name = k, s
			break
		}
	}
	name = normalizeToolName(name)
	if name == "" || name == NoTool || name == "no_tool" || name == "null" {
		return Decision{}
	}

	args := tools.Args{}
	found := false
	for _, k := range []string{"arguments", "args", "parameters", "params", "input"} {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		found = true
		// OpenAI-style: the arguments are a JSON document inside a string.
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			raw = json.RawMessage(asString)
		}
		args = tools.ParseArgs(raw)
		break
	}
	if !found {
		// Flattened: whatever is beside the tool's name is its arguments.
		for k, raw := range obj {
			if k == nameKey {
				continue
			}
			var v any
			if json.Unmarshal(raw, &v) == nil {
				args[k] = v
			}
		}
	}
	return Decision{Tool: name, Args: args}
}

func normalizeToolName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(s, "`\"'")
	return strings.NewReplacer(" ", "_", "-", "_", ".", "_").Replace(s)
}

// firstJSONValue returns the first complete JSON object or array in s, so
// leading prose does not cost the parse. The decoder finds the matching close,
// which is what keeps a brace inside a string from ending the value early.
func firstJSONValue(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var v json.RawMessage
	if err := dec.Decode(&v); err != nil {
		return ""
	}
	return string(v)
}

// stripFence removes a Markdown code fence around a reply.
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
