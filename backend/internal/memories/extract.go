package memories

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// MinExtractionChars is the threshold under which a turn is not sent to the
// model at all.
//
// Extraction is a second LLM call per turn, so it has to be worth making. The
// measure is the user's message and the assistant's answer together, trimmed,
// because a turn with something durable in it is one where the user said
// something about themselves *and* got a substantive reply. A greeting, a
// "thanks", an "ok that works" and their one-line answers all land far below
// 120 characters; "I prefer studying in the morning before class" (44) with any
// real answer clears it easily.
//
// The cost of the threshold is stated rather than hidden: a durable fact stated
// in six words and answered in six is missed. That is the trade being made --
// a memory table full of "the user said hello" is worse than a missed fact the
// user can restate, and every retrieved memory is spent from the same small
// context budget the documents compete for.
const MinExtractionChars = 120

// MaxFactsPerTurn caps one extraction. The prompt asks for 0-3; this is the
// enforcement, because a prompt is a request and a cap is a guarantee.
const MaxFactsPerTurn = 3

// MinKeptConfidence and MinKeptImportance drop a fact before it is stored.
//
// Confidence is the model's estimate that it read the fact rather than inferred
// it. Storing a 0.2-confidence reading means retrieving it into later prompts as
// a flat statement about the user, where nothing carries the doubt any more --
// which is how a memory system poisons itself.
//
// Importance is filtered too, and that is a decision taken from measurement
// rather than from first principles. Asked to extract from an ordinary lookup
// ("what do my field notes say about the tundra?"), llama3.2:3b reliably
// produces things like "The user inquired about their field notes" at
// importance 0.2 -- confident, accurate, and worthless. The model knows they
// are worthless; it says so in the field. Believing it is cheaper than a
// second filtering pass, and the failure mode is symmetrical: a genuinely
// important fact scored low is one the model was equally happy to discard.
const (
	MinKeptConfidence = 0.4
	MinKeptImportance = 0.4
)

// DuplicateSimilarity is how close a new fact has to be to an existing one to
// be treated as already known.
//
// It is high on purpose: this catches the same fact stated again in a later
// conversation ("I prefer studying in the morning" a second time), not two
// related facts about mornings. Below ~0.9 a genuine refinement ("the user
// prefers studying in the morning *before class*") would be swallowed by the
// vaguer memory it should have joined.
const DuplicateSimilarity = 0.95

// extractionSystemPrompt is the instruction half of the extraction call.
//
// It is a separate, much smaller prompt from the assistant's own: this call is
// a classifier, not a conversation, and giving it the grounding contract would
// invite it to answer the user's question a second time.
//
// The output contract is JSON, asked for in the prompt rather than enforced by
// the provider's JSON mode -- which measurably makes this worse on a 3B model;
// see Options.JSONMode. The one-line worked example is load-bearing: without it
// llama3.2:3b answers `[]` to every exchange, including ones that plainly state
// a preference. The one-fact-per-line fallback in the last paragraph is the
// salvage path for a model that produces neither; see ParseExtraction.
const extractionSystemPrompt = `You extract durable facts about a user from one exchange between that user and an assistant.

Read the exchange and list what is worth remembering about the user in the long term. Return between 0 and 3 facts.

Remember what the user says about themselves, in any part of their life, not only their work: what they like, dislike, prefer or avoid (food and diet, hobbies, routines, how they like to work), how they live (health, family, where they live), what they know or are learning, what they are working on, what they are trying to achieve, and what they have done. Something the user says they love, hate, enjoy or regularly do is not small talk.

The facts come from what the user said. The assistant's reply is there for context only -- an assistant saying it could not find something says nothing about whether the user stated a fact worth keeping.

Ignore the rest: the question they asked, what the assistant said, anything the assistant looked up in their documents or tasks, small talk, and anything you would be guessing at rather than reading. What a document says is not something the user knows or did: never write it as a fact about the user. A request for the assistant to do something is not something the user has done. If the exchange has none of the things worth remembering, return an empty list.

Write each fact as one short sentence in the third person, starting with "The user", and make it stand on its own: it will be read months later with none of this conversation around it.

Give each fact:
- "type": one of "episodic" (something that happened), "semantic" (a standing fact about the user), "preference" (something they like, dislike, prefer or avoid -- in food, lifestyle, hobbies or work), "project" (tied to a piece of ongoing work), "goal" (tied to something they are trying to achieve)
- "content": the sentence
- "importance": 0.0 to 1.0, how much this is worth keeping
- "confidence": 0.0 to 1.0, how sure you are that you read it from the exchange rather than inferred it

Reply with a JSON array and nothing else.

Example exchange:
User said: I switched my thesis topic to distributed consensus last week, and I am hopeless at getting started before lunch. Also, I can't stand spicy food, so skip the curry place.
Assistant replied: I could not find anything about your thesis or your eating habits in your documents or tasks. For spicy-free options, Italian or Japanese places are a safe bet.

Example reply:
[{"type":"project","content":"The user's thesis topic is distributed consensus.","importance":0.9,"confidence":0.95},{"type":"preference","content":"The user finds it hard to start work before lunch.","importance":0.6,"confidence":0.85},{"type":"preference","content":"The user dislikes spicy food.","importance":0.7,"confidence":0.9}]

If there is nothing durable, the reply is exactly []. If you cannot produce JSON, write one fact per line as: type | importance | confidence | sentence`

// ExtractionPrompt is the prompt sent to the model for one completed turn.
//
// The exchange is labelled and fenced rather than replayed as real user and
// assistant messages: this call is about the turn, not a continuation of it,
// and a model handed a bare user message tends to answer it again.
//
// The assistant's reply comes first and the user's message last, nearest the
// instruction. Measured on llama3.2:3b: "I'm vegetarian, so keep that in mind"
// answered by "I could not find anything about your dietary preferences" gave
// [] in every run with the user's message first -- the model took the reply's
// "found nothing" as its own answer -- and the fact in every run with it last.
func ExtractionPrompt(userMessage, assistantMessage string) []ai.Message {
	return extractionPromptFor(Turn{UserMessage: userMessage, AssistantMessage: assistantMessage})
}

// extractionPromptFor is ExtractionPrompt for a whole Turn. When the turn
// involved changes that have not happened, they are named after the exchange as
// things that are not facts -- the request, not a record of something done.
// The prompt is the request; RestatesUnconfirmed is the guarantee.
func extractionPromptFor(turn Turn) []ai.Message {
	body := "EXCHANGE\n\nAssistant replied:\n" +
		truncate(strings.TrimSpace(turn.AssistantMessage), MaxExchangeChars) +
		"\n\nUser said:\n" +
		truncate(strings.TrimSpace(turn.UserMessage), MaxExchangeChars)
	if len(turn.Unconfirmed) > 0 {
		body += "\n\nNOT FACTS\n\nThe user asked for these changes and they have NOT happened -- they are only proposed, or were rejected:"
		for _, c := range turn.Unconfirmed {
			body += "\n- " + truncate(strings.TrimSpace(c), MaxContentLen)
		}
		body += "\nA request is not something the user did, has or is. Write no fact about these changes."
	}
	return []ai.Message{
		{Role: ai.RoleSystem, Content: extractionSystemPrompt},
		{Role: ai.RoleUser, Content: body + "\n\nReturn the JSON array now."},
	}
}

// MaxExchangeChars truncates each half of the exchange shown to the extractor.
// A user message can be 8,000 characters and an answer longer still; the facts
// worth keeping are near the top of both, and a prompt that overflows the
// model's window is truncated by Ollama silently, which is worse.
const MaxExchangeChars = 4_000

// WorthExtracting reports whether a completed turn is substantial enough to
// spend a second model call on. See MinExtractionChars.
func WorthExtracting(userMessage, assistantMessage string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(userMessage))+
		utf8.RuneCountInString(strings.TrimSpace(assistantMessage)) >= MinExtractionChars
}

// wireCandidate is one fact as the model writes it. The scores are `score`
// rather than float64 because a model that has been told "0.0 to 1.0" will
// sometimes write "0.8" in quotes.
type wireCandidate struct {
	Type       string `json:"type"`
	Content    string `json:"content"`
	Importance score  `json:"importance"`
	Confidence score  `json:"confidence"`
	// Fact and Text are the two field names models reach for when they ignore
	// "content". Accepting them costs two lines and saves an extraction.
	Fact string `json:"fact"`
	Text string `json:"text"`
}

func (w wireCandidate) content() string {
	for _, s := range []string{w.Content, w.Fact, w.Text} {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

// score is a 0-1 number that may arrive as a JSON number, a quoted number, or
// not at all.
type score struct {
	Value float64
	Set   bool
}

func (s *score) UnmarshalJSON(data []byte) error {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "" || text == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// A score we cannot read is not a reason to lose the fact; it falls
		// back to the neutral default, and the fact is what matters.
		return nil
	}
	s.Value, s.Set = f, true
	return nil
}

// DefaultScore is what an omitted or unreadable importance/confidence becomes.
// It matches the column defaults in migration 000005: "no opinion", not "very
// sure".
const DefaultScore = 0.5

// ParseExtraction turns a model reply into candidate facts.
//
// It is deliberately lenient about shape and strict about content. The shapes
// accepted are a bare JSON array, a JSON object wrapping one under any key, a
// single JSON object, any of those with prose or a Markdown fence around them,
// and -- last -- the pipe-delimited line format the prompt names as a fallback.
// The reliability trade this makes is stated in docs/decisions.md: with the
// provider's JSON mode on, a 3B model returns parseable JSON nearly always; the
// leniency exists so that "nearly" does not cost a fact, and a reply that
// matches none of the shapes yields no candidates rather than a guess.
//
// What it will not do is invent: a fact with no text is dropped, an unreadable
// score becomes the neutral default rather than a confident one, and the cap of
// MaxFactsPerTurn is applied here rather than trusted to the prompt.
func ParseExtraction(reply string) []Candidate {
	raw := strings.TrimSpace(reply)
	if raw == "" {
		return nil
	}
	raw = stripFence(raw)

	wire := parseJSONCandidates(raw)
	if wire == nil {
		wire = parseLineCandidates(raw)
	}

	out := make([]Candidate, 0, MaxFactsPerTurn)
	seen := make(map[string]struct{}, MaxFactsPerTurn)
	for _, w := range wire {
		content := truncate(w.content(), MaxContentLen)
		if content == "" {
			continue
		}
		// The same sentence twice in one reply is one fact.
		key := strings.ToLower(content)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		out = append(out, Candidate{
			Type:       normalizeType(w.Type),
			Content:    content,
			Importance: clampScore(w.Importance),
			Confidence: clampScore(w.Confidence),
		})
		if len(out) == MaxFactsPerTurn {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseJSONCandidates finds the first JSON value in the reply and reads it as
// an array of facts, a wrapper object holding one, or a single fact.
func parseJSONCandidates(raw string) []wireCandidate {
	value := firstJSONValue(raw)
	if value == "" {
		return nil
	}

	var list []wireCandidate
	if err := json.Unmarshal([]byte(value), &list); err == nil {
		return list
	}

	// An object: either {"memories": [...]} under some key, or one bare fact.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &wrapper); err != nil {
		return nil
	}
	for _, member := range wrapper {
		if err := json.Unmarshal(member, &list); err == nil && len(list) > 0 {
			return list
		}
	}
	var single wireCandidate
	if err := json.Unmarshal([]byte(value), &single); err == nil && single.content() != "" {
		return []wireCandidate{single}
	}
	return nil
}

// firstJSONValue returns the first complete JSON array or object in s, so
// leading prose ("Here is the JSON:") does not cost the parse. It scans for a
// bracket and lets the decoder find the matching close, which is what keeps a
// brace inside a string from ending the value early.
func firstJSONValue(s string) string {
	start := strings.IndexAny(s, "[{")
	if start < 0 {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return ""
	}
	return string(value)
}

// parseLineCandidates reads the documented fallback format: one fact per line,
// `type | importance | confidence | sentence`.
//
// A line needs at least two fields to be considered, which is what stops prose
// ("I found nothing to remember.") from being read as a fact. Shorter forms are
// accepted because a model that falls back to this one has already shown it is
// not following instructions exactly: the last field is always the sentence.
func parseLineCandidates(raw string) []wireCandidate {
	var out []wireCandidate
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		w := wireCandidate{Type: parts[0], Content: parts[len(parts)-1]}
		if len(parts) >= 4 {
			_ = w.Importance.UnmarshalJSON([]byte(parts[1]))
			_ = w.Confidence.UnmarshalJSON([]byte(parts[2]))
		}
		out = append(out, w)
	}
	return out
}

// stripFence removes a Markdown code fence around a reply, which is the most
// common way a model wraps JSON it was asked for bare.
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

// normalizeType maps what the model wrote onto the closed set.
//
// An unrecognised type becomes `semantic` -- "a standing fact about the user" --
// rather than dropping the fact. The type is a facet the user filters on; it is
// not what retrieval matches on, so guessing it wrong costs a tidier list,
// while discarding the fact costs the fact.
func normalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, known := range Types {
		if t == known {
			return known
		}
	}
	return TypeSemantic
}

// clampScore keeps a score inside [0, 1]. A model that writes 5, or -1, or 80
// (meaning a percentage) must not put a value in the column that the CHECK
// constraint would reject and that comparisons would read as extreme.
func clampScore(s score) float64 {
	if !s.Set {
		return DefaultScore
	}
	switch {
	case s.Value < 0:
		return 0
	case s.Value > 1:
		return 1
	}
	return s.Value
}

// truncate cuts a string to at most max characters on a rune boundary.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:max]), " \t\n") + "…"
}

// AboutTheUser reports whether a proposed fact is a claim about the user at
// all.
//
// The prompt requires every fact to be one sentence starting with "The user",
// and this is where that is enforced rather than hoped for. It exists because
// of a measured failure: asked to extract from an ordinary lookup, llama3.2:3b
// answers with the retrieved material itself -- "The aurora borealis appeared
// over the tundra shortly after midnight" -- confidently and at high
// importance. That is a true sentence about a document, stored as a fact about
// a person, and it would then be recited back in later conversations as
// something the assistant knows about them.
//
// The test is deliberately loose: the word "user", anywhere, in any case. It
// admits "The user's thesis topic is distributed consensus" and rejects a
// sentence copied out of the context, which is the distinction that matters. A
// genuine fact phrased without the word is lost -- an acceptable price on a
// path where a wrong memory is far more expensive than a missing one, and one
// the prompt asks the model not to charge.
func AboutTheUser(content string) bool {
	return strings.Contains(strings.ToLower(content), "user")
}

// summarize is the one-line description of an extraction, for the log.
func summarize(candidates []Candidate) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		parts = append(parts, fmt.Sprintf("%s(%.2f)", c.Type, c.Confidence))
	}
	return strings.Join(parts, ", ")
}
