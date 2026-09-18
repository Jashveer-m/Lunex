package tools

import (
	"regexp"
	"strings"
	"unicode"
)

// ValueGrounded reports whether a value the model wrote for a grounded argument
// is one the user's message actually gives.
//
// A filter is the one kind of argument a model can get wrong silently. A bad
// title is shown to the user in the proposal and rejected; a bad date is an
// ArgumentError the user is asked about. A made-up filter is neither: it is a
// valid value, the search runs with it, and the user is told truthfully that
// nothing matched. That was measured -- llama3.2:3b routed "what do my notes
// say about seedlings?" to search_notes with `tag: "<unknown>"` and, on another
// message, `tag: "greenhouse"`, neither of which the user had said, and the
// searches came back empty. So a filter has to be read off the message rather
// than trusted: the routing call sees nothing but the message, so a value that
// is not in it was invented.
//
// For a closed set (Enum) the value is grounded when the message names it or
// one of its synonyms -- "finished tasks" grounds status completed. For free
// text the value has to appear in the message as whole words, and when the
// param has Cues one of those has to appear too, or the value written as a
// hashtag: "greenhouse" in "what do my greenhouse notes say" is a topic, and
// only "notes tagged greenhouse" or "#greenhouse" makes it a tag.
//
// It applies to a write's arguments too, when they are declared Grounded --
// see Param.Grounded for the measured case. It reports false for a param that
// is neither.
func ValueGrounded(p Param, value, message string) bool {
	if !p.MustBeGrounded() {
		return false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if len(p.Enum) > 0 {
		v := normalizeChoice(value, p.Enum, p.Synonyms)
		if v == "" {
			return false
		}
		return mentionsWords(message, v) || saidInOtherWords(p, v, message)
	}
	if mentionsWords(message, "#"+strings.TrimPrefix(value, "#")) {
		return true
	}
	if !mentionsWords(message, value) {
		// A value the message gives in other words, which for an open set is
		// the only way it can: "20 dollars" and "$20" both give USD, and
		// neither contains the code. Same rule the closed-set branch above
		// applies, for a param whose set is not closed.
		return saidInOtherWords(p, value, message)
	}
	if len(p.Cues) == 0 {
		return true
	}
	for _, cue := range p.Cues {
		if mentionsWords(message, cue) {
			return true
		}
	}
	return false
}

// saidInOtherWords reports whether the message names the value through one of
// the param's synonyms -- "dollars" or "$" for USD.
//
// A synonym with no letters or digits in it is a symbol, and is looked for as a
// plain substring: "$20" has no word boundary between the "$" and the number,
// so the word-boundary test that everything else uses would never match it.
func saidInOtherWords(p Param, value, message string) bool {
	for word, means := range p.Synonyms {
		if !strings.EqualFold(means, value) {
			continue
		}
		if isSymbol(word) {
			if strings.Contains(message, word) {
				return true
			}
			continue
		}
		if mentionsWords(message, word) {
			return true
		}
	}
	return false
}

// isSymbol reports whether s is punctuation rather than a word -- "$", "₹".
func isSymbol(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// mentionsWords reports whether phrase occurs in text as whole words, case and
// separator insensitively: "in_progress" is mentioned by "in progress".
func mentionsWords(text, phrase string) bool {
	phrase = strings.TrimSpace(strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(phrase)))
	if phrase == "" {
		return false
	}
	words := strings.Fields(phrase)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	text = strings.ToLower(strings.NewReplacer("_", " ", "-", " ").Replace(text))
	return regexp.MustCompile(`(^|[^\pL\pN#])` + strings.Join(words, `\s+`) + `($|[^\pL\pN])`).MatchString(text)
}
