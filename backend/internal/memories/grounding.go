package memories

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Turn is one completed exchange, as the extractor is allowed to read it.
//
// The two messages are the exchange. The other two fields are what the
// extractor must *not* turn into facts, and they exist because of three
// measured failures with the same root: extraction read something that was not
// confirmed reality about the user.
type Turn struct {
	UserMessage      string
	AssistantMessage string
	// Unconfirmed is what changes this conversation asked for that have not
	// happened -- proposed and waiting, rejected, or failed -- one entry per
	// change, written as the text of the change ("Book a flight to Delhi").
	// A rejected "book a flight to Delhi" was stored as "the user booked a
	// flight to Delhi", and a proposed task became a memory before anyone had
	// approved it. A change that was carried out is not in here: that one is
	// reality.
	Unconfirmed []string
	// Retrieved is the text of the document passages retrieved for the turn.
	// A question about an uploaded document produced "The user knows that tap
	// water stunted last year's seedlings" -- the document's content, phrased
	// as a fact about the user, which AboutTheUser admits because it contains
	// the word.
	Retrieved []string
}

// MinSignificantWordLen is the shortest word the two checks below compare.
// Shorter words are function words or noise ("to", "a", "is") far more often
// than they are the content of a fact.
const MinSignificantWordLen = 3

// stopWords never count as content. Besides the usual function words they
// include the vocabulary an extraction wraps around a fact rather than the fact
// itself: "The user knows that ...", "The user has read that ...". Leaving
// those in would make every fact about a document look partly the user's own.
var stopWords = setOf(
	"the", "and", "for", "with", "this", "that", "these", "those", "their", "they", "them",
	"his", "her", "its", "our", "your", "you", "she", "him", "has", "have", "had", "was",
	"were", "are", "been", "being", "will", "would", "can", "could", "should", "not", "but",
	"from", "into", "onto", "over", "than", "then", "about", "some", "any", "all", "also",
	"very", "who", "what", "which", "when", "where", "how", "why", "there", "here", "does",
	"did", "doing", "done", "just", "more", "most", "such", "only", "own", "same", "too",
	"user", "user's", "users", "know", "knows", "knew", "known", "learn", "learns", "learned",
	"learnt", "read", "reads", "say", "says", "said", "think", "thinks", "thought", "believe",
	"believes", "find", "finds", "found", "note", "noted", "aware", "understand", "understands",
	"according", "document", "documents", "mentioned", "mentions", "states", "stated",
	// The words a change's own text is built from, which say nothing about
	// what it is a change *to*.
	"task", "tasks", "goal", "goals", "note", "notes", "create", "add", "make", "remind",
)

func setOf(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

// significantWords is the content words of s, lower-cased, each once, in order.
func significantWords(s string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.Trim(w, "'")
		w = strings.TrimSuffix(w, "'s")
		if utf8.RuneCountInString(w) < MinSignificantWordLen {
			continue
		}
		if _, stop := stopWords[w]; stop {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
	}
	return out
}

// sameWord reports whether two words are the same word, allowing for the
// inflections an extraction introduces when it restates something: "book" and
// "booked", "seedling" and "seedlings". One must be a prefix of the other and
// the shorter at least four letters, so "run" does not match "rung".
func sameWord(a, b string) bool {
	if a == b {
		return true
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

// RestatesUnconfirmed reports whether a proposed fact is about a change that
// has not happened.
//
// The test is one shared content word with the change's own text. That is
// strict on purpose: the message that proposes a change is the request itself,
// so a "fact" sharing its subject -- "The user booked a flight to Delhi", "The
// user is planning a trip to Delhi" -- was read out of the request. A fact the
// same message states about something else ("I'm vegetarian -- add a task to
// buy tofu") shares nothing with the change and is kept. The cost is a real
// fact that happens to share a word with a pending or rejected change, which
// is the right way round on a path where a wrong memory is far more expensive
// than a missing one.
func RestatesUnconfirmed(content string, unconfirmed []string) bool {
	if len(unconfirmed) == 0 {
		return false
	}
	fact := significantWords(content)
	for _, change := range unconfirmed {
		for _, w := range significantWords(change) {
			if containsWord(fact, w) {
				return true
			}
		}
	}
	return false
}

// SaidByTheUser reports whether a proposed fact shares at least one content
// word with the user's own message.
//
// The prompt says facts come from what the user said, and a 3B model does not
// always comply. Measured on llama3.2:3b: told "I don't drink coffee" and
// answered with a suggestion of herbal tea, it stored "The user prefers herbal
// tea"; and, with the exchange shown reply-first, it once copied the prompt's
// own worked example into an unrelated turn -- "The user's thesis topic is
// distributed consensus" for a user who had said they were vegetarian. Neither
// shares a single word with what the user said. A fact the user stated in
// entirely different words ("I'm vegetarian" as "The user does not eat meat")
// is lost too; that is the trade, in the same direction as every other check
// here.
func SaidByTheUser(content, userMessage string) bool {
	said := significantWords(userMessage)
	for _, w := range significantWords(content) {
		if containsWord(said, w) {
			return true
		}
	}
	return false
}

// MinRestatedWords is the fewest content words a fact needs before it can be
// judged a restatement of a document. With fewer there is too little to tell a
// restatement from a coincidence.
const MinRestatedWords = 2

// RestatesRetrieved reports whether a proposed fact is the content of a
// retrieved document passage rather than something the user said.
//
// AboutTheUser checks the phrasing, and a model that phrases the document as a
// sentence about the user gets past it. This checks the content: of the fact's
// content words that can be traced to a source -- the retrieved passages or the
// user's own message -- count those found only in a passage. When that is at
// least half, most of what the fact says came from the document: "The user
// knows that tap water stunted last year's seedlings", asked "what does my
// garden guide say about the seedlings?", is five words from the guide and one
// from the question.
//
// Two words found only in a passage are enough on their own, whatever the
// share: they are content the user never said and the document did, and a
// fact about the user has no business carrying it. Measured: "The user's
// garden log recommends using collected rainwater for seedlings" names the log
// and the seedlings from the question, and "collected rainwater" from the log.
//
// Words traceable to neither do not count either way. They are the model's
// framing -- "prefers", "using" -- and counting them let "The user prefers
// using collected rainwater for seedlings" through, measured against the same
// guide, as two words of document in five.
//
// Words the user did say are never held against the fact, which is what keeps
// "I am vegetarian" stored when the retrieved recipe document is also about
// vegetarian cooking.
func RestatesRetrieved(content, userMessage string, retrieved []string) bool {
	if len(retrieved) == 0 {
		return false
	}
	fact := significantWords(content)
	if len(fact) < MinRestatedWords {
		return false
	}
	said := significantWords(userMessage)
	var passages []string
	for _, p := range retrieved {
		passages = append(passages, significantWords(p)...)
	}
	fromDocument, traced := 0, 0
	for _, w := range fact {
		inDocument, inMessage := containsWord(passages, w), containsWord(said, w)
		if inDocument || inMessage {
			traced++
		}
		if inDocument && !inMessage {
			fromDocument++
		}
	}
	return fromDocument >= MinRestatedWords || (traced >= MinRestatedWords && fromDocument*2 >= traced)
}
