package chat

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/study"
)

// Phase 10c: weak topics as a heuristic retrieval source.
//
// It is the calendar heuristic's kind of source, not a notification. Nothing
// here runs outside a turn, and nothing is surfaced unless the turn's own
// message gives a reason: it names a weak topic, or names the study plan or
// document a weak topic's questions came from, or asks how the user is doing
// or what to study. Anything else -- "what is on tomorrow" -- gets no weak
// topic in its context, however many there are.
//
// What the model is shown is the study service's own figures, written out in
// full. The grounding rule that goes with it (the study rule in prompt.go)
// says a topic is weak only when one of these sources says so, which is the
// other half of the same property: the assistant can only talk about weak
// areas the data has, and with the numbers the data has.

// WeakTopicLister is the study module's weak-topic read. Optional: a nil one is
// an assistant that retrieves exactly what 10b's did.
type WeakTopicLister interface {
	WeakTopics(ctx context.Context, userID uuid.UUID) ([]study.TopicStat, error)
}

// MaxWeakTopicSources is how many weak topics one answer sees. Three is "here
// is what to work on"; a longer list is a report, and a user who wants the
// whole of it asks, which routes to get_weak_topics.
const MaxWeakTopicSources = 3

// weakTopics is the heuristic. It skips itself when a tool already reported
// weak topics this turn -- the tool's are the same figures, and the same
// topic under two labels would split the citations between them.
func (s *Service) weakTopics(ctx context.Context, userID uuid.UUID, question string, first []Source) ([]Source, error) {
	if s.study == nil {
		return nil, nil
	}
	for _, src := range first {
		if src.Type == SourceWeakTopic {
			return nil, nil
		}
	}
	progress := asksAboutStudy(question)
	words := matchWords(question)
	if !progress && len(words) == 0 {
		// Nothing a topic, a plan or a document could be named by, and no
		// question about studying: no reason to look.
		return nil, nil
	}
	weak, err := s.study.WeakTopics(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("retrieve weak topics: %w", err)
	}
	picked := relevantWeakTopics(weak, words, progress)
	out := make([]Source, 0, len(picked))
	for _, t := range picked {
		out = append(out, weakTopicSource(t, ""))
	}
	return out, nil
}

// weakTopicSource renders one weak topic as a source. It has no id: a weak
// topic is a computation over answers, not a row a client can open -- the
// same standing a spending total has.
func weakTopicSource(t study.TopicStat, tool string) Source {
	return Source{
		Type: SourceWeakTopic, Title: t.Topic, Tool: tool,
		Excerpt: truncate(weakTopicSummary(t), MaxExcerptChars),
	}
}

// relevantWeakTopics picks which weak topics a message is about, worst first
// within each group.
//
// A topic is *named* when the message mentions it, or mentions a study plan or
// a document its questions came from. Named topics always come first. When the
// message is a question about studying in general, the rest follow, those
// under a still-active plan before the others -- which is the "recently active
// study plan" half of the rule: with no topic named, what the user is
// currently studying is the best guess at what they mean.
func relevantWeakTopics(weak []study.TopicStat, words map[string]struct{}, progress bool) []study.TopicStat {
	var named, active, rest []study.TopicStat
	for _, t := range weak {
		switch {
		case mentions(words, t.Topic) || mentionsAny(words, t.StudyPlans) || mentionsAny(words, t.Documents):
			named = append(named, t)
		case t.InActivePlan:
			active = append(active, t)
		default:
			rest = append(rest, t)
		}
	}
	out := named
	if progress {
		out = append(append(out, active...), rest...)
	}
	if len(out) > MaxWeakTopicSources {
		out = out[:MaxWeakTopicSources]
	}
	return out
}

// mentions reports whether a message's words name a phrase: every significant
// word of a one- or two-word phrase, or at least two of a longer one's.
//
// One shared word is not enough for a long phrase. "Mast feed timing" and
// "what's the timing of the meeting" share "timing", and a weak topic dropped
// into a calendar question on the strength of one common word is the noise
// this heuristic must not make.
func mentions(words map[string]struct{}, phrase string) bool {
	pw := matchWords(phrase)
	if len(pw) == 0 {
		return false
	}
	need := min(len(pw), 2)
	shared := 0
	for w := range pw {
		if _, ok := words[w]; ok {
			shared++
		}
	}
	return shared >= need
}

func mentionsAny(words map[string]struct{}, phrases []string) bool {
	for _, p := range phrases {
		if mentions(words, p) {
			return true
		}
	}
	return false
}

// matchStopWords never count towards a mention. They are grammar, the words
// for the study module's own things ("quiz", "plan", "topic"), and file
// extensions -- "relay-handbook.txt" is named by "relay" and "handbook", not
// by "txt".
var matchStopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "about": {}, "what": {}, "which": {},
	"how": {}, "should": {}, "would": {}, "could": {}, "can": {}, "you": {}, "your": {},
	"mine": {}, "are": {}, "was": {}, "were": {}, "did": {}, "doe": {}, "does": {},
	"doing": {}, "have": {}, "has": {}, "had": {}, "this": {}, "that": {}, "these": {},
	"those": {}, "from": {}, "into": {}, "get": {}, "got": {}, "getting": {}, "keep": {},
	"more": {}, "most": {}, "much": {}, "some": {}, "any": {}, "all": {}, "not": {},
	"quiz": {}, "quizze": {}, "question": {}, "topic": {}, "study": {}, "studying": {},
	"plan": {}, "txt": {}, "pdf": {}, "docx": {}, "doc": {}, "file": {}, "know": {},
	"tell": {}, "give": {}, "help": {}, "need": {}, "want": {}, "like": {}, "just": {},
	"really": {}, "still": {}, "again": {}, "next": {}, "now": {}, "today": {},
	"tonight": {}, "week": {}, "there": {}, "their": {}, "them": {}, "then": {},
	"than": {}, "when": {}, "where": {}, "who": {}, "why": {}, "will": {}, "wrong": {},
	"right": {}, "weak": {}, "part": {}, "one": {}, "off": {}, "out": {},
}

// matchWords is a text's significant words: lowercased, split on anything that
// is not a letter or a digit, at least three characters, a trailing plural "s"
// dropped so "timings" names "timing", and the stop words removed.
func matchWords(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = strings.TrimSuffix(w, "s")
		}
		if len([]rune(w)) < 3 {
			continue
		}
		if _, stop := matchStopWords[w]; stop {
			continue
		}
		out[w] = struct{}{}
	}
	return out
}

// studyPhrases are the ways of asking how studying is going or what to do next
// that name no topic. Matched as substrings of the lowercased message.
var studyPhrases = []string{
	"how am i doing", "how i'm doing", "how im doing", "how am i getting on",
	"how did i do", "how have i been doing", "how i've been doing", "my progress",
	"should i study", "should i revise", "should i focus", "should i work on",
	"should i practi", "study next", "revise next", "focus on", "work on next",
	"keep getting wrong", "getting wrong", "get wrong", "got wrong", "keep missing",
	"need to improve", "need to work on", "weak", "struggl", "worst at", "bad at",
}

// studyWords are whole words that make a message about studying.
var studyWords = map[string]struct{}{
	"study": {}, "studying": {}, "studied": {}, "revise": {}, "revising": {}, "revision": {},
	"practise": {}, "practice": {}, "practising": {}, "practicing": {}, "exam": {}, "exams": {},
	"quiz": {}, "quizzes": {},
}

// asksAboutStudy reports whether a message is about how the user's studying is
// going, or what to study, without necessarily naming a topic.
func asksAboutStudy(message string) bool {
	m := strings.ToLower(strings.Join(strings.Fields(message), " "))
	m = strings.ReplaceAll(m, "’", "'")
	for _, p := range studyPhrases {
		if strings.Contains(m, p) {
			return true
		}
	}
	for _, w := range strings.FieldsFunc(m, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if _, ok := studyWords[w]; ok {
			return true
		}
	}
	return false
}

// weakTopicSummary renders one weak topic as the lines the model is shown.
//
// Every number is written out and none is left for the model to derive: the
// counts, the wrong count, and the percentage, which is the study service's own
// rounding. "1 of 5 answers correct (20%)" is a sentence a 3B model repeats;
// "answers 5, correct 1" is one it divides, sometimes wrongly. The wrong share
// is written out too, percentage and all: llama3.2:3b, shown only the correct
// one, answered "25% were correct, meaning 75% were incorrect" -- right, but
// arithmetic the rule forbids, and the e2e run caught it.
func weakTopicSummary(t study.TopicStat) string {
	parts := []string{fmt.Sprintf("%d of %d answers correct (%d%%), %d of %d wrong (%d%%)",
		t.Correct, t.Answers, t.CorrectPercent(), t.Wrong(), t.Answers, 100-t.CorrectPercent())}
	if t.Quizzes == 1 {
		parts = append(parts, "across 1 quiz")
	} else {
		parts = append(parts, fmt.Sprintf("across %d quizzes", t.Quizzes))
	}
	if len(t.StudyPlans) > 0 {
		parts = append(parts, "under study plan "+quotedList(t.StudyPlans))
	}
	if len(t.Documents) > 0 {
		parts = append(parts, "from "+strings.Join(sortedCopy(t.Documents), ", "))
	}
	if !t.LastAnswered.IsZero() {
		parts = append(parts, "last answered "+t.LastAnswered.UTC().Format("Mon 2 Jan 2006"))
	}
	return strings.Join(parts, " · ")
}

func quotedList(items []string) string {
	q := make([]string, 0, len(items))
	for _, s := range sortedCopy(items) {
		q = append(q, strconv.Quote(s))
	}
	return strings.Join(q, ", ")
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
