package graph

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/memories"
)

// MinExtractionChars is the threshold under which a turn is not sent to the
// model at all.
//
// It is memories.MinExtractionChars, referenced rather than copied. The two
// extractions gate on the same measurement of the same input for the same
// reason -- a greeting and its one-line answer contain neither a durable fact
// nor a relationship, and both would cost a model call to discover that -- and
// an alias makes "these are one threshold" a compile-time fact rather than two
// constants that quietly drift apart. If a later phase finds that
// relationships need *more* text than facts do (they name two entities, not
// one), this is the line that splits, and the reason will be written here.
const MinExtractionChars = memories.MinExtractionChars

// MaxRelationshipsPerTurn caps one extraction. The prompt asks for 0-3; this
// is the enforcement, because a prompt is a request and a cap is a guarantee.
const MaxRelationshipsPerTurn = 3

// MinKeptConfidence drops a relationship before it is stored.
//
// It is higher than memories.MinKeptConfidence (0.4), and the difference is
// the point of setting it separately. A memory is filtered twice: on the
// model's confidence *and* on its importance, and the measured behaviour of
// llama3.2:3b is that the worthless extractions are the ones it scores low on
// importance while remaining perfectly confident about them. An edge has no
// importance column -- the schema gives it one score -- so the confidence
// floor is carrying both filters' weight on its own, and 0.5 is where it has
// to sit to do that.
//
// The cost is stated rather than hidden: a real relationship the model was
// only half sure it read is dropped. That is the trade. An edge is fed back
// into later prompts as a flat statement that two things are connected, with
// nothing left carrying the doubt -- which is how a knowledge graph poisons
// itself, and it is worse here than for a memory, because an edge also decides
// what *else* gets retrieved.
const MinKeptConfidence = 0.5

// MaxLabelWords bounds how many words a node's name may have.
//
// This is the graph's analogue of memories.AboutTheUser, and it exists for the
// same measured reason. Asked to name the entities in an exchange, llama3.2:3b
// will happily answer with a clause lifted out of the context -- "the aurora
// borealis appeared over the tundra shortly after midnight" as the name of a
// thing -- confidently and at a high score. A node is a name: "Go", "Alice",
// "the backend project", "systems programming coursework". Eight words is
// generous for every one of those and admits none of the sentences.
//
// A genuine entity with a nine-word name is lost. That is an acceptable price
// on a path where a wrong node is far more expensive than a missing one: a
// sentence stored as a node becomes a node the mention scan matches against
// every message, and then an edge the model is shown as a fact.
const MaxLabelWords = 8

// MaxExchangeChars truncates each half of the exchange shown to the extractor,
// for the same reason memories.MaxExchangeChars does: an overflowing prompt is
// truncated by Ollama silently, which is worse than truncating it here.
const MaxExchangeChars = 4_000

// extractionSystemPrompt is the instruction half of the extraction call.
//
// It is a separate call from the memory extraction rather than a second half
// of that one; docs/decisions.md has the argument. What matters here is that
// this prompt is a *relation* classifier and says so: it asks for triples, it
// names the closed sets both ends and the middle must come from, and it gives
// one worked example -- which is load-bearing for the same reason it is in the
// memory prompt, because a 3B model handed an unexampled schema answers `[]`
// to everything.
//
// The instruction to write the user as "the user" is what lets the resolver
// fold "I", "me" and "the user" onto one person node without guessing.
const extractionSystemPrompt = `You extract relationships between things from one exchange between a user and an assistant.

Read the exchange and list the connections it states. Return between 0 and 3 relationships.

Each relationship connects two entities. An entity is a NAME, never a sentence: a skill or subject ("Go", "linear algebra"), a person ("Alice", "the user"), or a piece of work ("the backend project", "the systems coursework"). Always write the user themselves as "the user".

Give each entity a type, one of:
- "skill": something learnable -- a language, a subject, a technique
- "person": a human being, including the user
- "project": a piece of work, a course, a goal, or anything being worked towards

Give each relationship a type, exactly one of: RELATED_TO, REQUIRES, DEPENDS_ON, WORKS_ON, KNOWS, INTERESTED_IN, STUDIES, COMPLETED, GOAL_OF.

Only list a connection the exchange actually states. The user asking a question about something does not connect them to it. Do not connect two things merely because they were mentioned in the same sentence. If the exchange states no connection, return an empty list.

Give each relationship:
- "from": the name of the first entity
- "from_type": its type
- "relationship": one of the types above
- "to": the name of the second entity
- "to_type": its type
- "confidence": 0.0 to 1.0, how sure you are that you read this from the exchange rather than inferred it

Reply with a JSON array and nothing else.

Example exchange:
User said: I am learning Rust so I can finish the compiler project this term, and Priya is helping me with it.
Assistant replied: Noted -- the compiler project is the only thing you have due this term.

Example reply:
[{"from":"the user","from_type":"person","relationship":"STUDIES","to":"Rust","to_type":"skill","confidence":0.95},{"from":"the compiler project","from_type":"project","relationship":"REQUIRES","to":"Rust","to_type":"skill","confidence":0.85},{"from":"Priya","from_type":"person","relationship":"WORKS_ON","to":"the compiler project","to_type":"project","confidence":0.8}]

If there is nothing to connect, the reply is exactly []. If you cannot produce JSON, write one relationship per line as: from | from_type | RELATIONSHIP | to | to_type | confidence`

// ExtractionPrompt is the prompt sent to the model for one completed turn.
//
// The exchange is labelled and fenced rather than replayed as real user and
// assistant messages, exactly as in memories.ExtractionPrompt: this call is
// about the turn, not a continuation of it.
func ExtractionPrompt(userMessage, assistantMessage string) []ai.Message {
	return []ai.Message{
		{Role: ai.RoleSystem, Content: extractionSystemPrompt},
		{Role: ai.RoleUser, Content: "EXCHANGE\n\nUser said:\n" +
			truncate(strings.TrimSpace(userMessage), MaxExchangeChars) +
			"\n\nAssistant replied:\n" +
			truncate(strings.TrimSpace(assistantMessage), MaxExchangeChars) +
			"\n\nReturn the JSON array now."},
	}
}

// WorthExtracting reports whether a completed turn is substantial enough to
// spend a model call on. See MinExtractionChars.
func WorthExtracting(userMessage, assistantMessage string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(userMessage))+
		utf8.RuneCountInString(strings.TrimSpace(assistantMessage)) >= MinExtractionChars
}

// wireEdge is one relationship as the model writes it. Every field has the
// aliases models reach for when they ignore the names the prompt gave them;
// accepting them costs a line each and saves an extraction.
type wireEdge struct {
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Source   string `json:"source"`
	FromType string `json:"from_type"`
	SubjType string `json:"subject_type"`

	To      string `json:"to"`
	Object  string `json:"object"`
	Target  string `json:"target"`
	ToType  string `json:"to_type"`
	ObjType string `json:"object_type"`

	Relationship string `json:"relationship"`
	Type         string `json:"type"`
	Relation     string `json:"relation"`
	Predicate    string `json:"predicate"`

	Confidence score `json:"confidence"`
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func (w wireEdge) from() string     { return firstNonEmpty(w.From, w.Subject, w.Source) }
func (w wireEdge) to() string       { return firstNonEmpty(w.To, w.Object, w.Target) }
func (w wireEdge) fromType() string { return firstNonEmpty(w.FromType, w.SubjType) }
func (w wireEdge) toType() string   { return firstNonEmpty(w.ToType, w.ObjType) }
func (w wireEdge) relationship() string {
	return firstNonEmpty(w.Relationship, w.Type, w.Relation, w.Predicate)
}

// score is a 0-1 number that may arrive as a JSON number, a quoted number, or
// not at all. It is the same three-way tolerance memories.score has, and for
// the same reason: a model told "0.0 to 1.0" sometimes writes "0.8" in quotes.
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
		// A score we cannot read is not a reason to lose the relationship; it
		// falls back to the neutral default.
		return nil
	}
	s.Value, s.Set = f, true
	return nil
}

// DefaultScore is what an omitted or unreadable confidence becomes. It matches
// the column default in migration 000006: "no opinion", not "very sure".
//
// It sits exactly on MinKeptConfidence, which means an omitted score is
// admitted and a score the model wrote *below* 0.5 is not. That boundary is
// the deliberate part. Dropping unscored relationships would look principled
// and would in practice mean that a model which simply does not emit the field
// -- a smaller one behind GRAPH_MODEL, a future provider -- extracts nothing at
// all, silently. An omitted score is a model that did not answer the question,
// not a model that answered "unsure", and only the second is evidence against.
const DefaultScore = 0.5

// ParseExtraction turns a model reply into candidate relationships.
//
// It is deliberately lenient about shape and strict about content, mirroring
// memories.ParseExtraction. The shapes accepted are a bare JSON array, a JSON
// object wrapping one under any key, a single JSON object, any of those inside
// prose or a Markdown fence, and -- last -- the pipe-delimited line format the
// prompt names as a fallback.
//
// What it will not do is invent. A triple missing either end is dropped, an
// unreadable confidence becomes the neutral default rather than a confident
// one, a relationship the allow-list does not have becomes RELATED_TO rather
// than a value the CHECK constraint would reject, and the cap of
// MaxRelationshipsPerTurn is applied here rather than trusted to the prompt.
func ParseExtraction(reply string) []Candidate {
	raw := strings.TrimSpace(reply)
	if raw == "" {
		return nil
	}
	raw = stripFence(raw)

	wire := parseJSONEdges(raw)
	if wire == nil {
		wire = parseLineEdges(raw)
	}

	out := make([]Candidate, 0, MaxRelationshipsPerTurn)
	seen := make(map[string]struct{}, MaxRelationshipsPerTurn)
	for _, w := range wire {
		from, to := NormalizeLabel(w.from()), NormalizeLabel(w.to())
		if from == "" || to == "" {
			continue
		}
		rel := NormalizeRelationship(w.relationship())
		// The same triple twice in one reply is one relationship.
		key := strings.ToLower(from) + "\x00" + rel + "\x00" + strings.ToLower(to)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}

		out = append(out, Candidate{
			From:         from,
			FromType:     NormalizeNodeType(w.fromType(), defaultFromType(rel)),
			To:           to,
			ToType:       NormalizeNodeType(w.toType(), defaultToType(rel)),
			Relationship: rel,
			Confidence:   clampScore(w.Confidence),
		})
		if len(out) == MaxRelationshipsPerTurn {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseJSONEdges finds the first JSON value in the reply and reads it as an
// array of relationships, a wrapper object holding one, or a single one.
func parseJSONEdges(raw string) []wireEdge {
	value := firstJSONValue(raw)
	if value == "" {
		return nil
	}

	var list []wireEdge
	if err := json.Unmarshal([]byte(value), &list); err == nil {
		return list
	}

	// An object: either {"relationships": [...]} under some key, or one bare
	// triple.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &wrapper); err != nil {
		return nil
	}
	for _, member := range wrapper {
		if err := json.Unmarshal(member, &list); err == nil && len(list) > 0 {
			return list
		}
	}
	var single wireEdge
	if err := json.Unmarshal([]byte(value), &single); err == nil && single.from() != "" && single.to() != "" {
		return []wireEdge{single}
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

// parseLineEdges reads the documented fallback format: one relationship per
// line, `from | from_type | RELATIONSHIP | to | to_type | confidence`.
//
// A line needs at least three fields, which is the minimum a triple can be
// written in and what stops prose ("I found no relationships.") from being read
// as one. Three fields are taken as `from | RELATIONSHIP | to`, because a
// model that has fallen back to this format has already shown it is not
// following the instruction exactly, and the two ends plus the middle are the
// part worth salvaging.
func parseLineEdges(raw string) []wireEdge {
	var out []wireEdge
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		var w wireEdge
		switch {
		case len(parts) >= 6:
			w = wireEdge{From: parts[0], FromType: parts[1], Relationship: parts[2],
				To: parts[3], ToType: parts[4]}
			_ = w.Confidence.UnmarshalJSON([]byte(parts[5]))
		case len(parts) == 5:
			w = wireEdge{From: parts[0], FromType: parts[1], Relationship: parts[2],
				To: parts[3], ToType: parts[4]}
		case len(parts) == 4:
			w = wireEdge{From: parts[0], FromType: parts[1], Relationship: parts[2], To: parts[3]}
		default:
			w = wireEdge{From: parts[0], Relationship: parts[1], To: parts[2]}
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

// NormalizeRelationship maps what the model wrote onto the closed set.
//
// An unrecognised relationship becomes RELATED_TO rather than dropping the
// edge, on the same reasoning that makes memories.normalizeType fall back to
// `semantic`: the pair is the finding, the label on the arrow is a facet. "The
// user and the compiler project are connected somehow" is a true and useful
// thing to have in the graph; discarding it because the model wrote "USES"
// costs the connection.
func NormalizeRelationship(r string) string {
	r = strings.ToUpper(strings.TrimSpace(r))
	r = strings.ReplaceAll(r, " ", "_")
	r = strings.ReplaceAll(r, "-", "_")
	for _, known := range Relationships {
		if r == known {
			return known
		}
	}
	return RelRelatedTo
}

// NormalizeNodeType maps what the model wrote onto the closed set, falling
// back to a type inferred from the relationship rather than to a fixed one.
//
// Only the three extracted types are reachable here: a conversation cannot
// name a task, goal, note or document into existence -- those nodes come from
// sync -- and letting the model claim `task` would create a node that says it
// mirrors a row and does not. A model that writes "task" gets `project`, which
// is the extracted type that means "a piece of work".
func NormalizeNodeType(t, fallback string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, known := range ExtractedTypes {
		if t == known {
			return known
		}
	}
	switch t {
	// The near-misses worth catching, rather than sending to the fallback:
	// they are unambiguous about which of the three the model meant.
	case NodeTask, NodeGoal, "work", "course", "job", "topic":
		return NodeProject
	case "human", "people", "contact":
		return NodePerson
	case "language", "technology", "tool", "subject", "concept":
		return NodeSkill
	}
	return fallback
}

// defaultFromType and defaultToType are what an unrecognised entity type falls
// back to, chosen from the relationship rather than fixed.
//
// It is a guess and it is labelled as one, but it is a much better guess than
// a constant: STUDIES and KNOWS point at things one learns, WORKS_ON points at
// work, and the left-hand side of a stated relationship is nearly always
// somebody. Guessing the type wrong costs a node filed under the wrong facet;
// dropping the triple costs the relationship, which is the thing being
// extracted.
func defaultFromType(rel string) string {
	switch rel {
	case RelRequires, RelDependsOn, RelGoalOf:
		return NodeProject
	default:
		return NodePerson
	}
}

func defaultToType(rel string) string {
	switch rel {
	case RelStudies, RelKnows, RelInterestedIn, RelRequires:
		return NodeSkill
	default:
		return NodeProject
	}
}

// clampScore keeps a confidence inside [0, 1]. A model that writes 5, or -1,
// or 80 (meaning a percentage) must not put a value in the column that the
// CHECK constraint would reject.
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

// Nameable reports whether a proposed label is a name rather than a sentence.
//
// This is the graph's quality gate on entity text; see MaxLabelWords for the
// measured failure it exists to stop. The test is a word count and a length,
// both loose: "the systems programming coursework" (4) passes, "the aurora
// borealis appeared over the tundra shortly after midnight" (9) does not.
// The length is measured *before* normalization, which is where this differs
// from the sync path. NormalizeLabel truncates at MaxLabelLen because a task
// title may legitimately run to validate.MaxTitleLen and its node still has to
// be stored; an extracted name over the limit is not a long name, it is a
// paragraph, and cutting it to two hundred characters would store the first
// two hundred characters of a paragraph as an entity.
func Nameable(label string) bool {
	if utf8.RuneCountInString(strings.TrimSpace(label)) > MaxLabelLen {
		return false
	}
	label = NormalizeLabel(label)
	if label == "" {
		return false
	}
	return len(strings.Fields(label)) <= MaxLabelWords
}

// MinGroundingWordLen is the shortest word that counts as evidence that an
// entity name came from the user's own message.
//
// It is MinMentionLen -- two -- and not one character more, which is a
// correction the tests made rather than a free choice: "Go" is the canonical
// entity of this whole phase and it is two letters, so a floor of three
// silently discards every relationship about it. Short *function* words are
// excluded by groundingStopWords instead, which is the distinction that
// actually matters.
const MinGroundingWordLen = MinMentionLen

// groundingStopWords never count as evidence, whatever their length. They are
// the words a model puts in an entity name for grammar rather than identity,
// and a label made only of them ("the this that") names nothing.
var groundingStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "this": {}, "that": {},
	"their": {}, "his": {}, "her": {}, "its": {}, "our": {}, "your": {},
	"some": {}, "any": {}, "new": {}, "old": {}, "about": {}, "from": {},
	"into": {}, "over": {}, "than": {}, "then": {}, "they": {}, "them": {},
	// The two-letter function words the floor above no longer excludes.
	"of": {}, "to": {}, "in": {}, "on": {}, "at": {}, "by": {}, "is": {},
	"it": {}, "as": {}, "an": {}, "or": {}, "be": {}, "do": {}, "so": {},
	"my": {}, "we": {}, "us": {}, "he": {}, "if": {}, "up": {},
}

// GroundedInMessage reports whether an entity name came from what the *user*
// said, rather than from the assistant's reply.
//
// This is the graph's counterpart to memories.AboutTheUser, and it exists for
// a failure measured by running the thing: asked to extract from a turn where
// the assistant had quoted the user's own documents back at them, llama3.2:3b
// returned "aurora borealis", "tundra" and "field notes from 14 March" as the
// entities -- names taken out of the retrieved context, wired together into
// confident relationships, and stored as things the assistant knows about the
// user's life. Every one of them passes Nameable: they are perfectly good
// names. They are just not what the exchange was about.
//
// The extraction prompt already says to read the exchange rather than the
// context, exactly as the memory prompt does. This is the enforcement, because
// a prompt is a request and a filter is a guarantee.
//
// The test is deliberately loose: one word of the label, at least
// MinGroundingWordLen long and not a stop word, appearing in the user's
// message as a whole word. That admits "the systems programming coursework"
// against "this term's systems programming coursework is in Rust", where no
// exact substring match exists, and rejects "aurora borealis" against a
// message that never mentions it.
//
// The cost is real and worth stating: a relationship where the user said "it"
// and the assistant supplied the name is lost. That is the trade -- on a path
// where a wrong edge is fed back into later prompts as a fact about the user
// and a missing one is restated the next time they mention it.
func GroundedInMessage(label, userMessage string) bool {
	// The user is always present in their own message, whatever words they
	// used to refer to themselves.
	if IsSelf(label) {
		return true
	}
	for _, word := range strings.Fields(strings.ToLower(NormalizeLabel(label))) {
		word = strings.Trim(word, `"'`+"`"+`.,;:!?()[]`)
		if utf8.RuneCountInString(word) < MinGroundingWordLen {
			continue
		}
		if _, stop := groundingStopWords[word]; stop {
			continue
		}
		if Mentions(userMessage, word) {
			return true
		}
	}
	return false
}

// truncate cuts a string to at most max characters on a rune boundary.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:max]), " \t\n") + "…"
}

// summarize is the one-line description of an extraction, for the log.
func summarize(candidates []Candidate) string {
	parts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		parts = append(parts, fmt.Sprintf("%s -%s-> %s (%.2f)", c.From, c.Relationship, c.To, c.Confidence))
	}
	return strings.Join(parts, ", ")
}
