package study

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The grounding check: does a proposed flashcard say what the document says?
//
// It is the inverse of internal/memories' grounding, and deliberately shaped
// like it. There, a "fact about the user" that turned out to be the content of
// a retrieved document is thrown away (RestatesRetrieved). Here, a card whose
// answer is *not* the content of the document is thrown away. Same machinery,
// opposite sign, because the two callers want opposite things from the same
// model: memory wants what the user said, study wants what the document said.
//
// Why this exists at all, rather than trusting the prompt. The prompt does say
// "use only the passages", and a 3B model does not always comply -- the
// measured failure in Phase 5 was llama3.2:3b answering from its own
// knowledge when the retrieved text was thin, confidently and in the same
// register as a real answer. A wrong memory is recoverable: the user reads it
// in the memory manager and deletes it. A wrong flashcard is worse, because
// the entire point of a flashcard is to be rehearsed until it is believed. So
// the prompt is the request and this is the guarantee.
//
// What it costs is real and is stated rather than hidden: a correct card
// phrased entirely in other words than the document's is dropped. "40" for
// "forty" is dropped. That is the right direction on a path where a plausible
// invention is far more expensive than a missing card, and the count of what
// was dropped is reported back (Proposal.Dropped) rather than swallowed.

// MinSignificantWordLen is the shortest word the check compares. Shorter words
// are function words or noise ("to", "a", "is") far more often than they are
// the content of an answer.
//
// A token containing a digit is kept whatever its length. That is the one
// difference from memories.MinSignificantWordLen, and it is the difference
// between the two jobs: "the user is vegetarian" does not turn on a number,
// and "how many bits in the tag field?" turns on nothing else.
const MinSignificantWordLen = 3

// MinGroundedWords is the fewest content words an answer needs before it is
// judged by share rather than by all-or-nothing. Below it there is too little
// to take a proportion of, so every word has to be found.
const MinGroundedWords = 2

// MinGroundedShare is how much of an answer has to come from the passages.
//
// Two thirds, not all of it: an answer written as a sentence carries framing
// the passage does not ("the filter is replaced before the resupply run" for a
// passage that says "needs a new fuel filter before the next resupply run"),
// and demanding every word would reject the model for writing English. Two
// thirds is enough that the substance has to be the document's while the
// glue can be the model's.
const MinGroundedShare = 2.0 / 3.0

// stopWords never count as content. Besides the usual function words they
// include the vocabulary a flashcard is *made* of rather than the vocabulary
// of its subject -- "what", "which", "define", "explain", "according" -- so
// that a question is judged on the thing it asks about and not on the asking.
var stopWords = setOf(
	"the", "and", "for", "with", "this", "that", "these", "those", "their", "they", "them",
	"his", "her", "its", "our", "your", "you", "she", "him", "has", "have", "had", "was",
	"were", "are", "been", "being", "will", "would", "can", "could", "should", "not", "but",
	"from", "into", "onto", "over", "than", "then", "about", "some", "any", "all", "also",
	"very", "who", "what", "which", "when", "where", "how", "why", "there", "here", "does",
	"did", "doing", "done", "just", "more", "most", "such", "only", "own", "same", "too",
	"many", "much", "each", "both", "other", "another", "between", "under", "after", "before",
	// The words a card is built out of, which say nothing about its subject.
	"define", "definition", "describe", "explain", "name", "list", "state", "give",
	"according", "document", "passage", "text", "mentioned", "mentions", "says", "said",
	"call", "called", "known", "term", "mean", "means", "meaning", "answer", "question",
)

func setOf(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

// SignificantWords is the content words of s, lower-cased, each once, in
// order. It is exported because the tests that pin the grounding rule are
// clearer when they can say which words a card was judged on.
func SignificantWords(s string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.Trim(w, "'")
		w = strings.TrimSuffix(w, "'s")
		if w == "" {
			continue
		}
		if !hasDigit(w) {
			if utf8.RuneCountInString(w) < MinSignificantWordLen {
				continue
			}
			if _, stop := stopWords[w]; stop {
				continue
			}
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// numberWords are the figures a model writes out in words. They are treated
// exactly like a token with a digit in it: matched only exactly, and fatal to
// an answer when the passages do not contain them.
//
// This is not a refinement, it is the hole the digit rule left. Measured, on
// llama3.2:3b against a handbook that says "the whole bank is equalised every
// forty days": the model wrote a question whose correct answer was "Every
// sixty days" -- and it was grounded, because "every" and "days" are both in
// the passage and two content words out of three clears MinGroundedShare.
// "sixty" has no digit in it, so the rule below that sinks an unsupported
// figure never fired.
//
// A wrong figure is the part of a card or a quiz a learner is least able to
// check and most likely to memorise, and for a quiz it is worse than wrong: it
// is the answer key, so it marks the learner wrong for knowing better. Whether
// the model wrote "40" or "forty" is not a difference in what is at stake.
var numberWords = setOf(
	"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen",
	"seventeen", "eighteen", "nineteen", "twenty", "thirty", "forty", "fifty",
	"sixty", "seventy", "eighty", "ninety", "hundred", "thousand", "million", "billion",
	// Ordinals, which is how a passage names a step or a section.
	"first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth",
	"ninth", "tenth", "eleventh", "twelfth", "twentieth", "thirtieth", "fortieth",
	"fiftieth", "hundredth", "thousandth",
	// Quantities that are figures in everything but spelling.
	"half", "quarter", "third", "twice", "double", "triple", "dozen", "once",
)

// isFigure reports whether a word states a quantity -- as digits, or written
// out. Both are held to the same two rules: matched exactly, and fatal when
// the passages do not contain them.
func isFigure(s string) bool {
	if hasDigit(s) {
		return true
	}
	_, ok := numberWords[s]
	return ok
}

// sameWord reports whether two words are the same word, allowing for the
// inflections a restatement introduces: "last" and "lasted", "filter" and
// "filters". One must be a prefix of the other and the shorter at least four
// letters, so "run" does not match "rung".
//
// A figure has to match exactly, whether it is written in digits or in words.
// "40" is a prefix of "400", and a card answering "400 metres" from a passage
// that says "40 metres" is precisely the failure this file exists to catch --
// and "four" is a prefix of "fourteen", which is the same failure spelled out.
func sameWord(a, b string) bool {
	if a == b {
		return true
	}
	if isFigure(a) || isFigure(b) {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return utf8.RuneCountInString(a) >= 4 && strings.HasPrefix(b, a)
}

func containsWord(words []string, w string) bool {
	for _, x := range words {
		if sameWord(x, w) {
			return true
		}
	}
	return false
}

// passageWords is every content word of the passages, flattened.
func passageWords(passages []string) []string {
	var out []string
	for _, p := range passages {
		out = append(out, SignificantWords(p)...)
	}
	return out
}

// GroundedIn reports whether text is drawn from the passages.
//
// At least MinGroundedShare of its content words have to appear in them, with
// the all-or-nothing rule below MinGroundedWords: a two-word answer with one
// word from the document is half invented, and a proportion is the wrong tool
// for judging it.
//
// Text with no content words at all is not grounded. That is not a technicality
// -- "It depends." is a real thing a model writes on the back of a card.
func GroundedIn(text string, passages []string) bool {
	return groundedIn(text, passageWords(passages))
}

func groundedIn(text string, source []string) bool {
	words := SignificantWords(text)
	if len(words) == 0 {
		return false
	}
	found := 0
	for _, w := range words {
		if containsWord(source, w) {
			found++
			continue
		}
		// A figure the passages do not contain sinks the answer outright,
		// whatever the share works out to.
		//
		// The share is the right measure for words, because an answer written
		// as a sentence carries some of the model's own. It is the wrong
		// measure for a number: "400 volts after 12 seconds", from a passage
		// that says 40 volts after 12 seconds, is three quarters the
		// document's and entirely wrong, and a figure is the part of a
		// flashcard a learner is least able to check and most likely to
		// memorise. An unsupported number is never framing.
		//
		// "Every sixty days" against a passage that says "every forty days" is
		// the same sentence with the same arithmetic, and it is why isFigure
		// covers words as well as digits; see numberWords.
		if isFigure(w) {
			return false
		}
	}
	if len(words) < MinGroundedWords {
		return found == len(words)
	}
	return float64(found) >= MinGroundedShare*float64(len(words))
}

// MentionsAny reports whether text names at least one thing the passages talk
// about. It is the weaker test the *question* has to pass.
func MentionsAny(text string, passages []string) bool {
	return mentionsAny(text, passageWords(passages))
}

func mentionsAny(text string, source []string) bool {
	for _, w := range SignificantWords(text) {
		if containsWord(source, w) {
			return true
		}
	}
	return false
}

// CardGrounded reports whether a proposed card traces to the passages.
//
// The two sides are held to different standards, on purpose.
//
// The back is the claim -- it is what the learner will end up believing -- so
// it has to be GroundedIn the passages.
//
// The front only has to name something in them (MentionsAny). A question is
// mostly the words of asking, which say nothing about the source: "What did
// the aurora do, and for how long?" is four content words of which one is the
// document's, and holding it to the answer's share would reject a perfectly
// grounded card for being well phrased. What the weaker test still catches is
// the card that is about something else entirely -- a question about
// photosynthesis attached to an answer stitched out of a networking paper.
func CardGrounded(c NewCard, passages []string) bool {
	source := passageWords(passages)
	return groundedIn(c.Back, source) && mentionsAny(c.Front, source)
}

// QuestionGrounded reports whether a proposed quiz question traces to the
// passages. It is CardGrounded with the multiple-choice shape substituted in,
// and deliberately the same two tests rather than a second rule: the back of a
// card and the correct option of a question are the same thing -- the sentence
// the learner will end up believing -- so they are held to the same bar.
//
// The correct answer has to be GroundedIn the passages. The question itself
// only has to name something in them (MentionsAny), for the reason the front
// of a card does.
//
// The distractors are not checked at all, and that is the one place this
// differs from a flashcard in substance rather than in shape. A wrong answer
// is *supposed* to be wrong: requiring it to appear in the document would mean
// every option was something the document says, which is the opposite of what
// a distractor is for, and refusing options the document does not contain
// would throw away every well-made question. What that costs is real and is
// stated rather than hidden: nothing here can tell a good distractor from one
// that happens to be true of a part of the document the model was not shown.
// The mitigation is the same one the whole phase rests on -- the user reads
// every question, with its options, on the approval card before the quiz
// exists.
//
// A question whose correct index does not point at an option is not grounded
// rather than being an error here: CorrectAnswer returns "" for it, and "" has
// no content words. Validation catches it properly, with a field name; this is
// the backstop.
func QuestionGrounded(q NewQuestion, passages []string) bool {
	source := passageWords(passages)
	return groundedIn(q.CorrectAnswer(), source) && mentionsAny(q.Question, source)
}
