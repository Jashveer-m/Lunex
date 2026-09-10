package graph

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the entity-resolution half of the graph: how two mentions of
// the same name are recognised as one node, and how a chat message is matched
// against the nodes a user already has.
//
// The matching is exact on a normalized label -- trimmed, internal whitespace
// collapsed, compared case-insensitively -- and it is not fuzzy. That is the
// decision, and it is taken deliberately rather than for want of a library.
//
// Nodes carry no embeddings in this phase, so "fuzzy" here would mean edit
// distance or trigram similarity, and both merge names that are genuinely
// different: Go and Godot, Alice and Alicia, "OS coursework" and "OS
// homework". A wrong merge in a graph is worse than a duplicate node, because
// it does not just lose a distinction -- it *fabricates* relationships, giving
// Godot every edge Go has and then feeding them back into the model as
// context. A duplicate node is visible, harmless and deletable; an invented
// edge is a false claim about the user that reads exactly like a true one.
//
// Case folding and whitespace collapsing are the normalizations that pay for
// themselves, because they catch the duplication that actually happens: the
// same model writes "Go" in one turn and "go" in the next, and "backend
// project" with two spaces once. Everything past that is left to a later
// phase, where node embeddings would let the merge be proposed with a score
// rather than guessed at with a string metric. See docs/decisions.md.

// NormalizeLabel puts a label in the form the graph stores and compares.
//
// It does not lowercase: the stored label is what a client displays, so "Go"
// stays "Go". The comparison is case-insensitive in SQL (lower(label)) and in
// FoldLabel here, which is the same thing said in the two places it has to be
// said.
func NormalizeLabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	// Models like to write the entity in quotes, or with a trailing full stop
	// or comma from the sentence it was lifted out of.
	//
	// Sentence punctuation is stripped from the right only. Leading
	// punctuation is usually part of the name -- ".NET", "#golang", "@priya"
	// -- and trimming both ends would file .NET under NET, which is a
	// different thing and a node the user could never find again.
	s = strings.TrimRight(s, `"'`+"`"+` .,;:!?`)
	s = strings.TrimLeft(s, `"'`+"`"+` `)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > MaxLabelLen {
		s = strings.TrimSpace(string([]rune(s)[:MaxLabelLen]))
	}
	return s
}

// FoldLabel is the comparison form: normalized, then lowercased. It is what
// SelfAliases is keyed by and what the mention scan compares.
func FoldLabel(s string) string { return strings.ToLower(NormalizeLabel(s)) }

// IsSelf reports whether a label is one of the ways a model refers to the user.
func IsSelf(label string) bool {
	_, ok := SelfAliases[FoldLabel(label)]
	return ok
}

// MinMentionLen is the shortest label the mention scan will match on.
//
// A one-character label ("R", "C") appears inside almost every message, so
// matching on it would attach a node to every question the user asks. Two is
// the floor because "Go" is two characters and is exactly the case the phase
// is meant to handle.
const MinMentionLen = 2

// Mentions reports whether text names label, as a whole word rather than as a
// substring.
//
// The boundary test is what makes this usable at all: without it "Go" matches
// "going", "algorithm" and "Django", and a graph lookup fires on every second
// message. The boundaries are "not a letter or a digit" on both sides, so
// "Go's" and "(Go)" and "Go." all match while "Golang" does not -- a
// deliberate near-miss, since Golang is a name the user can have a node for on
// its own.
func Mentions(text, label string) bool {
	label = FoldLabel(label)
	if utf8.RuneCountInString(label) < MinMentionLen {
		return false
	}
	hay := strings.ToLower(text)
	for from := 0; from < len(hay); {
		i := strings.Index(hay[from:], label)
		if i < 0 {
			return false
		}
		start := from + i
		if boundaryBefore(hay, start) && boundaryAfter(hay, start+len(label)) {
			return true
		}
		// Advance by one byte rather than by the match, so overlapping
		// occurrences are all considered.
		from = start + 1
	}
	return false
}

// boundaryBefore reports whether the rune ending at byte offset i is a word
// boundary; the start of the string is one.
func boundaryBefore(s string, i int) bool {
	if i <= 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !wordRune(r)
}

// boundaryAfter is the same test at the far end of a match.
func boundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return !wordRune(r)
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
