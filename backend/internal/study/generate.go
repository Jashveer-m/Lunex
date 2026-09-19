package study

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
)

// generationSystemPrompt is the instruction half of the generation call.
//
// It is a separate, much smaller prompt from the assistant's own: this call
// writes cards from a passage, it does not converse, and giving it the chat
// grounding contract would invite it to answer the user instead.
//
// The output contract is JSON, asked for in the prompt rather than enforced by
// the provider's JSON mode -- which measurably makes this worse on a 3B model,
// the same finding Phase 5 recorded for extraction; see Options.JSONMode. The
// worked example is load-bearing for the same reason it is there: without one,
// llama3.2:3b writes a prose summary of the passage and no cards at all. The
// one-card-per-line fallback in the last paragraph is the salvage path for a
// model that produces neither; see ParseFlashcards.
//
// The example is about bread, and that is deliberate rather than whimsical.
// The grounding check downstream would happily pass a card the model copied
// straight out of this prompt if the example's subject overlapped the document
// being studied -- and then the check would be measuring nothing. So the
// example's vocabulary is kept clear of anything the tests or the end-to-end
// script upload, and a test pins that.
//
// Every instruction about *where the content comes from* is repeated in three
// places -- the opening line, the rules, and the last sentence before the
// example -- because that is the one thing the grounding check will enforce
// afterwards, and a card the check drops is a card this prompt failed to get.
const generationSystemPrompt = `You write flashcards from a passage of somebody's own study material.

You will be given numbered passages from one document. Write flashcards that test what those passages say, and nothing else.

Rules:
- Every answer must be stated in the passages. Do not use anything you know from outside them. If the passages do not say it, do not write a card about it.
- Use the passages' own words and numbers for the answer. Do not round a figure, rename a term, or translate a unit.
- The front is one short question. The back is the answer to it, in one or two sentences.
- Name the thing in the question. "What is it used for?" is not a card; "How often is the starter fed?" is.
- One fact per card, and no two cards on the same fact.
- If the passages will not support the number of cards asked for, write fewer. Fewer real cards is the correct answer; do not pad.

Give each card:
- "front": the question
- "back": the answer, as the passages state it

Reply with a JSON array and nothing else.

Example passages:
` + examplePassages + `

Example reply:
` + exampleReply + `

If the passages support no cards at all, the reply is exactly []. If you cannot produce JSON, write one card per line as: question | answer`

// examplePassages and exampleReply are the worked example, pulled out of the
// prompt so a test can hold *them* to the no-shared-vocabulary rule rather
// than the whole prompt -- which necessarily shares ordinary words like
// "short" and "question" with everything.
const examplePassages = `[1] Sourdough starter is kept at room temperature and fed twice a day: equal weights of flour and water, discarding half the starter each time.
[2] A loaf is ready to shape once the dough has roughly doubled, which takes four hours at 24 degrees.`

const exampleReply = `[{"front":"How often is a sourdough starter fed at room temperature?","back":"Twice a day, with equal weights of flour and water."},{"front":"How long does the dough take to roughly double at 24 degrees?","back":"Four hours."}]`

// GenerationPrompt is the prompt sent to the model for one document.
//
// The passages are numbered and fenced rather than run together, for two
// reasons. A numbered list is what the worked example shows, so the shape the
// model is asked to read matches the shape it was shown; and the numbering is
// what lets a person reading the action log put a card next to the text it
// came from.
//
// The instruction comes last, after the passages, which is the ordering
// measured to work in Phase 5: a model handed material and then told what to
// do with it does the thing, and one told first and then handed material
// tends to answer about the material.
func GenerationPrompt(passages []documents.Passage, filename, topic string, count int) []ai.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "PASSAGES from %s\n", filename)
	if topic != "" {
		// Named so the model writes about the part of the document the user
		// asked about. It is not a licence to bring in outside knowledge of
		// the topic, and the sentence says so.
		fmt.Fprintf(&b, "\nThe user asked for cards about %q. Only the passages below may be used, whatever else you know about it.\n", topic)
	}
	for _, p := range PromptPassages(passages) {
		fmt.Fprintf(&b, "\n[%d] %s\n", p.ChunkIndex+1, strings.TrimSpace(p.Content))
	}
	fmt.Fprintf(&b, "\nWrite up to %s from these passages. Return the JSON array now.",
		plural(count, "flashcard"))
	return []ai.Message{
		{Role: ai.RoleSystem, Content: generationSystemPrompt},
		{Role: ai.RoleUser, Content: b.String()},
	}
}

// PromptPassages is the passages that fit in one prompt: at most
// PassagesPerGeneration of them and at most MaxGroundingChars of text.
//
// It is exported and used for the grounding check as well as for the prompt,
// which is the property that matters: the text a card is checked against is
// exactly the text the model was shown. Checking against the whole document
// would pass cards drawn from a part of it the model never saw -- which is not
// possible, so it would only ever mean the check was measuring something else.
func PromptPassages(passages []documents.Passage) []documents.Passage {
	out := make([]documents.Passage, 0, PassagesPerGeneration)
	budget := MaxGroundingChars
	for _, p := range passages {
		if len(out) == PassagesPerGeneration || budget <= 0 {
			break
		}
		if n := utf8.RuneCountInString(p.Content); n > budget {
			p.Content = truncate(p.Content, budget)
		}
		budget -= utf8.RuneCountInString(p.Content)
		out = append(out, p)
	}
	return out
}

// PassageTexts is the text of some passages, which is what the grounding check
// takes.
func PassageTexts(passages []documents.Passage) []string {
	out := make([]string, 0, len(passages))
	for _, p := range passages {
		out = append(out, p.Content)
	}
	return out
}

// wireCard is one card as the model writes it. Every field has the aliases a
// model reaches for when it ignores the two it was given; accepting them costs
// two lines each and saves a generation.
type wireCard struct {
	Front    string `json:"front"`
	Back     string `json:"back"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Q        string `json:"q"`
	A        string `json:"a"`
	Prompt   string `json:"prompt"`
	Response string `json:"response"`
	Term     string `json:"term"`
	// Definition is the one that matters most in practice: asked for cards
	// from a glossary-shaped passage, a model writes {"term": ..., "definition": ...}
	// rather than front/back about half the time.
	Definition string `json:"definition"`
}

func (w wireCard) front() string { return firstNonEmpty(w.Front, w.Question, w.Q, w.Prompt, w.Term) }
func (w wireCard) back() string {
	return firstNonEmpty(w.Back, w.Answer, w.A, w.Response, w.Definition)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// ParseFlashcards turns a model reply into candidate cards.
//
// It is deliberately lenient about shape and strict about content, the same
// posture as memories.ParseExtraction. The shapes accepted are a bare JSON
// array, a JSON object wrapping one under any key, a single JSON object, any
// of those with prose or a Markdown fence around them, and -- last -- the
// pipe-delimited line format the prompt names as a fallback. A reply that
// matches none of them yields no cards rather than a guess.
//
// What it will not do is invent or complete. A card missing either side is
// dropped rather than filled in: a question with no answer is not a card that
// needs finishing, it is a card that would be shown to somebody trying to
// learn with nothing on the back. The same sentence twice in one reply is one
// card. Nothing here checks whether a card is *true* of the document -- that
// is CardGrounded, and it runs after this.
func ParseFlashcards(reply string, limit int) []NewCard {
	raw := strings.TrimSpace(reply)
	if raw == "" {
		return nil
	}
	raw = stripFence(raw)

	wire := parseJSONCards(raw)
	if wire == nil {
		wire = parseLineCards(raw)
	}
	if limit <= 0 || limit > MaxCardsPerBatch {
		limit = MaxCardsPerBatch
	}

	out := make([]NewCard, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, w := range wire {
		card, err := ValidateCard(NewCard{Front: w.front(), Back: w.back()})
		if err != nil {
			continue
		}
		// One fact per card was asked for; the same *question* twice is the
		// model restating rather than a second fact, whatever it put on the
		// back.
		key := strings.ToLower(card.Front)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, card)
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseJSONCards finds the first JSON value in the reply and reads it as an
// array of cards, a wrapper object holding one, or a single card.
func parseJSONCards(raw string) []wireCard {
	value := firstJSONValue(raw)
	if value == "" {
		return nil
	}

	var list []wireCard
	if err := json.Unmarshal([]byte(value), &list); err == nil {
		return list
	}

	// An object: either {"flashcards": [...]} under some key, or one bare card.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &wrapper); err != nil {
		return nil
	}
	for _, member := range wrapper {
		if err := json.Unmarshal(member, &list); err == nil && len(list) > 0 {
			return list
		}
	}
	var single wireCard
	if err := json.Unmarshal([]byte(value), &single); err == nil && single.front() != "" {
		return []wireCard{single}
	}
	return nil
}

// firstJSONValue returns the first complete JSON array or object in s, so
// leading prose ("Here are the flashcards:") does not cost the parse. It scans
// for a bracket and lets the decoder find the matching close, which is what
// keeps a brace inside a string from ending the value early.
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

// parseLineCards reads the documented fallback format: one card per line,
// `question | answer`.
//
// A line needs exactly the two fields to be considered, and both have to hold
// something. That is stricter than the memories fallback, which takes the last
// field of whatever it finds, and the reason is the difference in what a bad
// parse costs: a mangled memory is one wrong sentence in a list the user can
// read, and a mangled card is a question whose answer is the leftovers of a
// bullet point. A numbered or bulleted line is still read -- a model that has
// fallen back to this one is already not following instructions exactly -- so
// the leading "1." or "- " is stripped rather than made to fail.
func parseLineCards(raw string) []wireCard {
	var out []wireCard
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*•0123456789.) \t")
		parts := strings.Split(line, "|")
		if len(parts) != 2 {
			continue
		}
		front, back := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if front == "" || back == "" {
			continue
		}
		out = append(out, wireCard{Front: front, Back: back})
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

// truncate cuts a string to at most max characters on a rune boundary.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:max]), " \t\n") + "…"
}

// plural renders a count with its noun: "1 flashcard", "8 flashcards".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
