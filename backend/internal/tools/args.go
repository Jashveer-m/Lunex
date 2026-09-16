package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Args are the arguments a model wrote for a tool call, before anything about
// them is trusted.
//
// Reading them is lenient about shape and strict about content, the same
// posture as the Phase 5 and Phase 6 extraction parsers. Shape: a number where
// a string was asked for, a comma-separated string where a list was, an alias
// for a parameter's name, a placeholder like "none" meaning "not given" -- all
// accepted, because each costs a line and rejecting them costs the call.
// Content is the tool's own business, and it validates with the same rules the
// HTTP API applies.
//
// An argument the tool does not declare is ignored rather than rejected. It
// cannot do anything -- the canonical input is built from the declared ones
// only -- and a model that adds a stray "user_id" has not thereby gained a way
// to name one.
type Args map[string]any

// ParseArgs decodes a JSON object into Args. Anything but an object, including
// null, is no arguments at all.
func ParseArgs(raw json.RawMessage) Args {
	var a Args
	if err := json.Unmarshal(raw, &a); err != nil || a == nil {
		return Args{}
	}
	return a
}

// placeholders are the values a model writes for an argument it has nothing to
// put in. They mean "not given", never the literal word.
var placeholders = map[string]struct{}{
	"": {}, "none": {}, "null": {}, "nil": {}, "n/a": {}, "na": {}, "-": {},
	"unknown": {}, "not specified": {}, "unspecified": {},
}

// isPlaceholder reports whether s is a placeholder, including one written as a
// template slot -- "<unknown>", "[none]" -- which is how llama3.2:3b echoes the
// `<tool name>` in the routing prompt's reply format.
func isPlaceholder(s string) bool {
	_, empty := placeholders[strings.ToLower(strings.TrimSpace(strings.Trim(s, "<>[]{}")))]
	return empty
}

// String returns the first of the named arguments that holds a value, as a
// trimmed string. Numbers and booleans are rendered rather than dropped.
func (a Args) String(names ...string) string {
	for _, name := range names {
		v, ok := a[name]
		if !ok {
			continue
		}
		s := ""
		switch t := v.(type) {
		case string:
			s = t
		case float64:
			s = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			s = strconv.FormatBool(t)
		default:
			continue
		}
		s = strings.TrimSpace(s)
		if isPlaceholder(s) {
			continue
		}
		return s
	}
	return ""
}

// Strings returns the first of the named arguments that holds a list: a JSON
// array of strings, or one string separated by commas.
func (a Args) Strings(names ...string) []string {
	for _, name := range names {
		v, ok := a[name]
		if !ok {
			continue
		}
		var out []string
		switch t := v.(type) {
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok {
					out = append(out, s)
				} else if item != nil {
					out = append(out, fmt.Sprint(item))
				}
			}
		case string:
			out = strings.Split(t, ",")
		}
		kept := out[:0]
		for _, s := range out {
			s = strings.TrimSpace(s)
			if !isPlaceholder(s) {
				kept = append(kept, s)
			}
		}
		if len(kept) > 0 {
			return kept
		}
	}
	return nil
}

// normalizeChoice maps what the model wrote onto a closed set, through a table
// of the near-misses worth catching. It returns "" for anything it cannot place,
// and the caller decides whether that is an error or an omission.
func normalizeChoice(v string, allowed []string, synonyms map[string]string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.NewReplacer("-", "_", " ", "_").Replace(v)
	for _, a := range allowed {
		if v == a {
			return a
		}
	}
	if s, ok := synonyms[v]; ok {
		return s
	}
	return ""
}

var prioritySynonyms = map[string]string{
	"urgent": "high", "important": "high", "critical": "high", "top": "high", "asap": "high",
	"normal": "medium", "med": "medium", "moderate": "medium", "regular": "medium",
	"lowest": "low", "minor": "low", "whenever": "low",
}

var taskStatusSynonyms = map[string]string{
	"done": "completed", "complete": "completed", "finished": "completed", "closed": "completed",
	"inprogress": "in_progress", "started": "in_progress",
	"doing": "in_progress", "active": "in_progress", "ongoing": "in_progress", "working": "in_progress",
	"todo": "pending", "to_do": "pending", "open": "pending", "not_started": "pending", "new": "pending",
}

var goalStatusSynonyms = map[string]string{
	"done": "completed", "complete": "completed", "achieved": "completed", "finished": "completed",
	"open": "active", "current": "active", "in_progress": "active", "ongoing": "active",
	"dropped": "abandoned", "cancelled": "abandoned", "canceled": "abandoned", "given_up": "abandoned",
}

var goalTypeSynonyms = map[string]string{
	"short": "short_term", "shortterm": "short_term", "long": "long_term", "longterm": "long_term",
	"work": "career", "job": "career", "professional": "career",
	"learning": "education", "study": "education", "school": "education", "academic": "education",
	"money": "financial", "finance": "financial", "savings": "financial", "saving": "financial",
	"health": "personal", "fitness": "personal", "hobby": "personal", "life": "personal",
	"projects": "project",
}
