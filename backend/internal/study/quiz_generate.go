package study

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
)

// quizGenerationSystemPrompt is the instruction half of the quiz generation
// call.
//
// It is generate.go's prompt with the output shape changed and one instruction
// added, and the shape of the file is deliberately the same: rules, then a
// worked example, then a named fallback format. Everything the flashcard
// prompt's comment says about why -- the example is load-bearing, JSON is
// asked for rather than enforced, the grounding instruction is repeated three
// times because it is the one a checker will enforce afterwards -- holds here
// unchanged.
//
// The added instruction is about the wrong answers, and it is the only part of
// this prompt with no counterpart in the flashcard one. A distractor has a job:
// it has to be about the same thing as the right answer and it has to be
// wrong. A model left to itself writes "None of the above" and "All of these",
// which test nothing, or writes three options that are obviously silly, which
// tests nothing either. Nothing downstream can check this -- a distractor is
// not grounded in the document by definition -- so unlike the grounding rule,
// this one really is only a request. See QuestionGrounded.
//
// The example reuses the flashcard prompt's passages, which is the point:
// the same two sentences, a different output shape. Their vocabulary is kept
// clear of anything the tests upload, so a model that copied the example
// verbatim could not sail through the grounding check, and a test pins it.
const quizGenerationSystemPrompt = `You write multiple-choice quiz questions from a passage of somebody's own study material.

You will be given numbered passages from one document. Write questions about what those passages say, and nothing else.

Rules:
- Every correct answer must be stated in the passages. Do not use anything you know from outside them. If the passages do not say it, do not write a question about it.
- Use the passages' own words and numbers for the correct answer. Do not round a figure, rename a term, or translate a unit.
- Give four options. Exactly one is correct.
- The wrong options must be about the same thing as the right one and must be clearly wrong: a different number, a different part, a different interval. Never write "none of the above", "all of the above", "both A and B", or an option that is obviously not a real answer.
- Name the thing in the question. "What is it used for?" is not a question; "How often is the starter fed?" is.
- One fact per question, and no two questions on the same fact.
- If the passages will not support the number of questions asked for, write fewer. Fewer real questions is the correct answer; do not pad.

Give each question:
- "question": the question
- "options": the four choices, as an array of strings
- "correct_index": which option is correct, counting from 0
- "topic": two or three words naming what the question is about

Reply with a JSON array and nothing else.

Example passages:
` + examplePassages + `

Example reply:
` + exampleQuizReply + `

If the passages support no questions at all, the reply is exactly []. If you cannot produce JSON, write one question per line as: question | option | option | option, with a * in front of the correct option.`

// exampleQuizReply is the worked example's output, pulled out of the prompt so
// a test can hold it to the no-shared-vocabulary rule.
//
// Its distractors are what the rule above asks for: same subject, plainly
// wrong. That matters more than it looks -- the example is the strongest
// instruction in the prompt, and an example with a lazy "none of the above" in
// it would teach the model to write them whatever the rules said.
const exampleQuizReply = `[{"question":"How often is a sourdough starter fed at room temperature?","options":["Once a week","Twice a day","Every third hour","Only before baking"],"correct_index":1,"topic":"starter feeding"},` +
	`{"question":"How long does the dough take to roughly double at 24 degrees?","options":["Four hours","Ten hours","One week","Overnight"],"correct_index":0,"topic":"bulk fermentation"}]`

// QuizGenerationPrompt is the prompt sent to the model for one document.
//
// Same construction as GenerationPrompt, and for the same measured reasons:
// the passages are numbered so a person reading the action log can put a
// question next to the text it came from, and the instruction comes after the
// material rather than before it.
func QuizGenerationPrompt(passages []documents.Passage, filename, topic string, count int) []ai.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "PASSAGES from %s\n", filename)
	if topic != "" {
		fmt.Fprintf(&b, "\nThe user asked to be quizzed on %q. Only the passages below may be used, whatever else you know about it.\n", topic)
	}
	for _, p := range PromptPassages(passages) {
		fmt.Fprintf(&b, "\n[%d] %s\n", p.ChunkIndex+1, strings.TrimSpace(p.Content))
	}
	fmt.Fprintf(&b, "\nWrite up to %s from these passages. Return the JSON array now.",
		plural(count, "multiple-choice question"))
	return []ai.Message{
		{Role: ai.RoleSystem, Content: quizGenerationSystemPrompt},
		{Role: ai.RoleUser, Content: b.String()},
	}
}

// wireQuestion is one question as the model writes it.
//
// Every field has the aliases a model reaches for when it ignores the four it
// was given, on the same terms as wireCard: two lines each, and each one saves
// a generation. The three that carry the answer are json.RawMessage rather
// than a type, because a model writes the correct answer as a number, as a
// letter and as the option's own text about equally often, and which of those
// it chose is not knowable until it is read.
type wireQuestion struct {
	Question  string          `json:"question"`
	Prompt    string          `json:"prompt"`
	Q         string          `json:"q"`
	Text      string          `json:"text"`
	Stem      string          `json:"stem"`
	Options   json.RawMessage `json:"options"`
	Choices   json.RawMessage `json:"choices"`
	RawAnswer json.RawMessage `json:"answer"`
	// Correct and CorrectAnswer are the two the model reaches for when it does
	// not write correct_index; CorrectIndex is the one that was asked for.
	Correct       json.RawMessage `json:"correct"`
	CorrectAnswer json.RawMessage `json:"correct_answer"`
	CorrectIndex  json.RawMessage `json:"correct_index"`
	AnswerIndex   json.RawMessage `json:"answer_index"`
	Topic         string          `json:"topic"`
	Tag           string          `json:"tag"`
	Subject       string          `json:"subject"`
	Area          string          `json:"area"`
}

func (w wireQuestion) question() string {
	return firstNonEmpty(w.Question, w.Prompt, w.Q, w.Text, w.Stem)
}

func (w wireQuestion) topic() string {
	return firstNonEmpty(w.Topic, w.Tag, w.Subject, w.Area)
}

// ParseQuizQuestions turns a model reply into candidate questions.
//
// It is the quiz counterpart of ParseFlashcards and has the same posture --
// lenient about shape, strict about content -- with one extra job: working out
// which option the model meant. A question whose correct answer cannot be
// resolved to exactly one of its options is dropped rather than guessed at,
// because the guess would be a quiz that marks a right answer wrong, which is
// worse than a quiz with one fewer question in it.
//
// Nothing here checks whether a question is *true* of the document. That is
// QuestionGrounded, and it runs after this.
func ParseQuizQuestions(reply string, limit int) []NewQuestion {
	raw := strings.TrimSpace(reply)
	if raw == "" {
		return nil
	}
	raw = stripFence(raw)

	wire := parseJSONQuestions(raw)
	if wire == nil {
		wire = parseLineQuestions(raw)
	}
	if limit <= 0 || limit > MaxQuestionsPerQuiz {
		limit = MaxQuestionsPerQuiz
	}

	out := make([]NewQuestion, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, w := range wire {
		options := parseOptions(w.Options, w.Choices)
		index, ok := correctIndexOf(w, options)
		if !ok {
			continue
		}
		q, err := ValidateQuestion(NewQuestion{
			Question: w.question(), Options: options, CorrectIndex: index, Topic: w.topic(),
		})
		if err != nil {
			continue
		}
		// One fact per question was asked for; the same question twice is the
		// model restating rather than a second fact, whatever it chose as the
		// answer.
		key := strings.ToLower(q.Question)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, q)
		if len(out) == limit {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseJSONQuestions finds the first JSON value in the reply and reads it as
// an array of questions, a wrapper object holding one, or a single question.
// Same three shapes parseJSONCards accepts, for the same reason.
func parseJSONQuestions(raw string) []wireQuestion {
	value := firstJSONValue(raw)
	if value == "" {
		return nil
	}

	var list []wireQuestion
	if err := json.Unmarshal([]byte(value), &list); err == nil && len(list) > 0 {
		return list
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &wrapper); err != nil {
		return nil
	}
	for _, member := range wrapper {
		if err := json.Unmarshal(member, &list); err == nil && len(list) > 0 && list[0].question() != "" {
			return list
		}
	}
	var single wireQuestion
	if err := json.Unmarshal([]byte(value), &single); err == nil && single.question() != "" {
		return []wireQuestion{single}
	}
	return nil
}

// parseOptions reads the choices, in the order they are to be shown.
//
// Three shapes are accepted: an array of strings, an array of anything (a
// model asked for four numbers writes four numbers), and an object keyed by
// letter -- {"A": "...", "B": "..."} -- which is read in key order, because
// that is the order it was written in and the order the letters mean. A label
// the model put in front of the text ("B) eleven seconds") is stripped, so the
// option reads as an option and so matching an answer written as text against
// it works.
func parseOptions(values ...json.RawMessage) []string {
	for _, raw := range values {
		if len(raw) == 0 {
			continue
		}
		var list []any
		if err := json.Unmarshal(raw, &list); err == nil {
			if out := labelledStrings(list); len(out) > 0 {
				return out
			}
			continue
		}
		var byLetter map[string]any
		if err := json.Unmarshal(raw, &byLetter); err != nil {
			continue
		}
		keys := make([]string, 0, len(byLetter))
		for k := range byLetter {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make([]any, 0, len(keys))
		for _, k := range keys {
			ordered = append(ordered, byLetter[k])
		}
		if out := labelledStrings(ordered); len(out) > 0 {
			return out
		}
	}
	return nil
}

func labelledStrings(values []any) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		s := ""
		switch t := v.(type) {
		case string:
			s = t
		case float64:
			s = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			s = strconv.FormatBool(t)
		case nil:
			continue
		default:
			continue
		}
		out = append(out, stripOptionLabel(s))
	}
	return out
}

// stripOptionLabel removes an "A) ", "b. " or "3) " the model put in front of
// an option's text.
func stripOptionLabel(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	rest := s
	head := rest[0]
	isLetter := (head >= 'A' && head <= 'F') || (head >= 'a' && head <= 'f')
	isDigit := head >= '0' && head <= '9'
	if !isLetter && !isDigit {
		return s
	}
	rest = strings.TrimLeft(rest[1:], " ")
	switch {
	case strings.HasPrefix(rest, ")"), strings.HasPrefix(rest, "."), strings.HasPrefix(rest, ":"):
		rest = strings.TrimSpace(rest[1:])
	default:
		return s
	}
	if rest == "" {
		// The label was the whole option. Keeping it is better than returning
		// nothing -- validation will decide -- but it is not a label.
		return s
	}
	return rest
}

// correctIndexOf works out which option the model meant, and reports whether
// it could.
//
// The fields split into two kinds, and keeping them apart is what stops the
// one wrong answer this function can give. `answer` and `correct` name the
// answer *by value*; `correct_index` and `answer_index` name it *by position*.
// A quiz whose options are themselves numbers -- "2", "4", "6", "8" -- is
// where the difference bites: `correct_index: 2` means the third option, and
// reading it as the text "2" would mark the first one right.
//
// Within that, the order is how reliable each form is rather than how it was
// documented. A model writes the option's text correctly far more often than
// it counts the options, so a by-value field wins over an index when both are
// present and they disagree. A letter is next, because "B" can only mean one
// thing whichever field it arrived in. A bare position is read last, and only
// when it is in range: an index pointing at no option is not salvaged by
// assuming the model counted from one, because that assumption is right about
// as often as it is wrong and being wrong means a quiz that marks the right
// answer wrong.
func correctIndexOf(w wireQuestion, options []string) (int, bool) {
	if len(options) == 0 {
		return 0, false
	}
	byValue := []json.RawMessage{w.RawAnswer, w.Correct, w.CorrectAnswer}
	byIndex := []json.RawMessage{w.CorrectIndex, w.AnswerIndex}

	// The answer written out, from a field that means the answer itself. Only
	// a JSON string is read this way -- a number in `answer` is a position.
	for _, raw := range byValue {
		if s, ok := jsonString(raw); ok {
			if i, found := matchOptionText(s, options); found {
				return i, true
			}
		}
	}
	// A letter, from either kind of field.
	for _, raw := range append(append([]json.RawMessage{}, byValue...), byIndex...) {
		if s, ok := jsonString(raw); ok {
			if i, found := matchOptionLetter(s, options); found {
				return i, true
			}
		}
	}
	// A position, from a field that means one first.
	for _, raw := range append(append([]json.RawMessage{}, byIndex...), byValue...) {
		s, ok := scalarString(raw)
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n >= 0 && n < len(options) {
			return n, true
		}
	}
	return 0, false
}

// jsonString is the value of a field the model wrote as a JSON string, and
// false for a number, an object, an array or a field it left out.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// scalarString renders a JSON scalar as a string, and reports false for
// anything else -- an object, an array, or a field the model omitted.
func scalarString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// matchOptionText matches an answer written out in full against the options.
// It matches on exactly one, so a value that is a substring of two of them
// resolves to neither.
func matchOptionText(value string, options []string) (int, bool) {
	value = collapse(stripOptionLabel(value))
	if value == "" {
		return 0, false
	}
	for i, o := range options {
		if strings.EqualFold(collapse(o), value) {
			return i, true
		}
	}
	found, matches := 0, 0
	for i, o := range options {
		if strings.EqualFold(strings.TrimRight(collapse(o), "."), strings.TrimRight(value, ".")) {
			found, matches = i, matches+1
		}
	}
	if matches == 1 {
		return found, true
	}
	return 0, false
}

// matchOptionLetter reads an answer written as "B", "b)" or "(C)".
func matchOptionLetter(value string, options []string) (int, bool) {
	v := strings.Trim(strings.TrimSpace(value), "()[].: ")
	if len(v) != 1 {
		return 0, false
	}
	c := v[0]
	switch {
	case c >= 'A' && c <= 'Z':
		c -= 'A'
	case c >= 'a' && c <= 'z':
		c -= 'a'
	default:
		return 0, false
	}
	if int(c) >= len(options) {
		return 0, false
	}
	return int(c), true
}

// parseLineQuestions reads the documented fallback format: one question per
// line, `question | option | option | option`, with a `*` in front of the
// correct option.
//
// It is stricter than the flashcard fallback in the way that matters: a line
// with no marked option, or with more than one, yields nothing rather than a
// question whose answer is a guess. A numbered or bulleted line is still read
// -- a model that has fallen back to this is already not following
// instructions exactly.
func parseLineQuestions(raw string) []wireQuestion {
	var out []wireQuestion
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*•0123456789.) \t")
		parts := strings.Split(line, "|")
		if len(parts) < 1+MinOptions {
			continue
		}
		question := strings.TrimSpace(parts[0])
		if question == "" {
			continue
		}
		options := make([]string, 0, len(parts)-1)
		correct, marked := 0, 0
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, "*") {
				correct, marked = len(options), marked+1
				p = strings.TrimSpace(strings.TrimPrefix(p, "*"))
			}
			options = append(options, stripOptionLabel(p))
		}
		if marked != 1 {
			continue
		}
		encoded, err := json.Marshal(options)
		if err != nil {
			continue
		}
		out = append(out, wireQuestion{
			Question: question, Options: encoded,
			CorrectIndex: json.RawMessage(strconv.Itoa(correct)),
		})
	}
	return out
}
