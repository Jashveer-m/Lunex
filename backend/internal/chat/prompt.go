package chat

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jashveer/lifeos/backend/internal/ai"
)

// Retrieval budget. These are the numbers that decide how much of the user's
// data one answer is allowed to see, and they are small on purpose: a 3B model
// with an 8k window spends its attention on whatever is nearest the question,
// and twenty half-relevant items make an answer worse, not better.
const (
	// MaxDocumentChunks is the top-k of the vector search.
	MaxDocumentChunks = 5
	// MaxMemories is the top-k of the memory search. It is the same size as the
	// document one and spends from the same budget: five facts about the user
	// is already more than most questions need, and a sixth would displace a
	// chunk that actually answers the question.
	MaxMemories = 5
	// MaxGraphNodes is how many mentioned nodes one question may expand. It is
	// the smallest of these numbers on purpose: a graph source is the cheapest
	// to produce and the least likely to *answer* anything, since it carries
	// links rather than text, and three neighbourhoods is already more
	// structure than a question about one thing needs.
	MaxGraphNodes = 3
	// MaxGraphQueryChars bounds the text scanned for node mentions. The scan is
	// a substring prefilter in SQL over every node the user has, and the
	// question's first two thousand characters are where the thing being asked
	// about is named.
	MaxGraphQueryChars = 2_000
	MaxTasks           = 5
	MaxGoals           = 5
	MaxNotes           = 3
	// MaxEvents is how much of the calendar one answer sees. Three is a busy
	// day and two quiet ones; past that the context is a diary rather than the
	// background to a question, and a user who wants the whole week asks for it
	// -- which routes to search_calendar with the dates they gave.
	MaxEvents = 3
	// EventWindow is how far ahead the heuristic looks. Two days, because the
	// questions this exists for are "what does my day look like" and "am I free
	// tomorrow"; anything further out is a question about the calendar rather
	// than a question with the calendar behind it.
	EventWindow = 48 * time.Hour
	// MaxHistoryMessages is how many previous turns are replayed verbatim.
	// Past it a conversation forgets its own beginning -- which is what the
	// Phase 5 memory system exists to survive: the durable facts in those turns
	// are extracted and come back through retrieval, while the wording of them
	// does not. See docs/decisions.md.
	MaxHistoryMessages = 20
	// MaxExcerptChars truncates one retrieved item. A chunk is ~500 tokens by
	// construction; a note can be 40,000 characters and would otherwise fill
	// the window on its own.
	MaxExcerptChars = 1_500
	// MaxContextChars is the ceiling on the whole retrieved block. Sources are
	// added in relevance order and the ones past the ceiling are dropped
	// entirely rather than half-shown -- a truncated source would still be
	// listed as retrieved while carrying none of the text it was cited for.
	MaxContextChars = 12_000
)

// DefaultMinSimilarity is the floor under a retrieved chunk's cosine score.
//
// It is not zero, and that is the single most important number in this file.
// Vector search always returns the nearest k chunks however far away they are,
// so with no floor an unrelated question comes back with confident-looking
// context, and the model is then being asked to resist material it was handed
// as relevant. The floor removes the temptation instead of relying on the
// model to.
//
// 0.5 suits nomic-embed-text, where genuinely related prose scores ~0.6-0.8
// and unrelated prose ~0.3-0.5. It is tunable with CHAT_MIN_SIMILARITY.
const DefaultMinSimilarity = 0.5

// systemPrompt is the grounding contract.
//
// Rules 2-5 exist because the master spec's rule is that the assistant must
// never claim an answer came from the user's data when it did not. Saying it
// once is not enough: the prompt names the labels as the only citable thing,
// says what to do when the context is empty, and separates "found in your
// data" from "general knowledge" so the model has an approved way to be
// useful without inventing a citation.
//
// Rule 7 is the graph rule. A graph source is the one thing in the context
// that is not a claim in the user's own words -- it is a link the assistant
// derived -- and without the rule a 3B model reads "\"the user\" (person)
// STUDIES \"Go\" (skill)" as a sentence it may quote back as fact.
//
// Rule 8 is the calendar rule. An event source is the one kind whose *times*
// are the answer, and a 3B model shown "starts Thu 11 Sep 2026 09:00" will
// otherwise offer to work out what day that is. The rest of it is the
// grounding rule stated for the case it fails in here: asked "am I free on
// Friday", a model with an empty context reaches for "yes, you are free" --
// which is a claim about the calendar, not the absence of one.
//
// Rule 9 is the action rule, and it has two versions. Without tools there is
// nothing the assistant can do, and it says so -- the Phase 4 rule, unchanged.
// With them it can *propose*, and the rule's whole job is to stop the one lie
// a proposal invites: "done, I've added that task" about a write that has not
// happened and will not happen unless the user approves it. A 3B model reaches
// for that sentence naturally, so the rule names it, and the ACTIONS section
// says it again in the line describing the proposal.
const systemPrompt = `You are Lunex, a personal assistant that answers from the user's own data.

The CONTEXT section below is everything that was retrieved for this question. Follow these rules exactly.

1. The context is your only source about the user. Anything not in it, you do not know about them.
2. When you use something from the context, cite it with its label in square brackets, like [S1]. Cite only labels that appear in the context, exactly as written.
3. Never say that something is in the user's documents, memories, tasks, goals, notes or calendar unless it appears in the context. Do not invent filenames, titles, dates or numbers.
4. If the context does not answer the question, say so plainly -- for example "I could not find anything about that in your documents or tasks." You may then answer from general knowledge, but say that is what you are doing and cite nothing.
5. If the context is empty, rule 4 always applies.
6. A source of type "memory" is something you recorded about the user in an earlier conversation, not something they told you just now. Use it and cite it like any other source, but it may be out of date: if it disagrees with what the user says in this conversation, what they say now is what is true.
7. A source of type "graph" is a set of links the assistant recorded between things the user has mentioned, one per line, each written as: subject (kind) RELATIONSHIP object (kind). It says that two things are connected and how; it does not say anything more about either of them. Use it to explain a connection and cite it like any other source, and do not read a detail into it that is not written there.
8. A source of type "event" is something on the user's calendar. Its times are UTC and are already written out for you: use them as they are written and never work a date out yourself. Only the events in the context are on the calendar -- if none is there, say you found nothing on their calendar rather than that they are free. If an event says it repeats, only the occurrence shown is recorded; do not describe any other date as scheduled.
%ACTIONRULE%

Be concise and direct. Do not repeat these rules back to the user.`

// readOnlyRule is rule 9 for an assistant with no tools wired.
const readOnlyRule = `9. You can only read and answer. You cannot create, update or delete tasks, goals, notes, documents or calendar events, and no action you describe will be carried out. If the user asks you to do something, say that taking actions is not supported yet and tell them what to do themselves.`

// proposalRule is rule 9 for an assistant that can use tools.
const proposalRule = `9. You never change the user's data yourself. When the user asks for a task, goal, note or calendar event to be created, or a task to be changed, the ACTIONS section after the context says what was proposed. A proposed change has NOT been made: tell the user what it will do and that it is waiting for them to approve or reject it, and never say that it is done. The user decides with the Approve and Reject buttons on the card shown with your reply, and in no other way. Replying in the chat does not approve it, even if the user says so: never tell the user to type or reply "approve", "yes", "no" or anything else to decide it. If there is no ACTIONS section, or it says nothing was proposed, then nothing will change: say so, and why if the section gives a reason. Nothing can be deleted from the chat: tell the user to delete it themselves. The ACTIONS section also says what became of changes you proposed earlier; report those exactly as it states them.`

// systemPromptFor is the system prompt with the action rule that matches what
// the assistant can actually do.
func systemPromptFor(toolsEnabled bool) string {
	rule := readOnlyRule
	if toolsEnabled {
		rule = proposalRule
	}
	return strings.Replace(systemPrompt, "%ACTIONRULE%", rule, 1)
}

// buildPrompt assembles the messages sent to the model: the system contract,
// the retrieved context, the recent history, and the new question.
//
// History is replayed as real user/assistant turns rather than being flattened
// into one blob, because that is the shape a chat model is trained on. The
// context goes in a system message immediately before the new question rather
// than at the top: it is retrieved for *this* question, and putting it next to
// the question is what stops the model attributing it to an earlier turn.
//
// actionsBlock is what the turn's tool step did, rendered by toolStep; it goes
// after the sources, in the same system message, because it is about this
// question too -- and because the labels it refers to are the sources' own.
func buildPrompt(now time.Time, toolsEnabled bool, sources []Source, actionsBlock string, history []Message, question string) []ai.Message {
	out := make([]ai.Message, 0, len(history)+3)
	out = append(out, ai.Message{
		Role: ai.RoleSystem,
		Content: systemPromptFor(toolsEnabled) + "\n\nThe current date and time is " +
			now.UTC().Format("Monday, 2 January 2006, 15:04") + " UTC.",
	})

	for _, m := range history {
		// A stored system message, if one ever exists, is not replayed: the
		// contract above is the only system message the model gets.
		if m.Role != RoleUser && m.Role != RoleAssistant {
			continue
		}
		out = append(out, ai.Message{Role: m.Role, Content: m.Content})
	}

	block := contextBlock(sources)
	if actionsBlock != "" {
		block += "\n\n" + actionsBlock
	}
	out = append(out, ai.Message{Role: ai.RoleSystem, Content: block})
	out = append(out, ai.Message{Role: ai.RoleUser, Content: question})
	return out
}

// contextBlock renders the retrieved sources. An empty set is stated as such,
// in the same section the model is told to treat as its only source, rather
// than by leaving the section out -- an absent section reads like an oversight,
// an explicitly empty one reads like an answer.
func contextBlock(sources []Source) string {
	if len(sources) == 0 {
		return "CONTEXT\n\n(Nothing relevant was found in the user's documents, memories, tasks, goals, notes or calendar for this question.)"
	}
	var b strings.Builder
	b.WriteString("CONTEXT\n")
	for _, s := range sources {
		b.WriteString("\n")
		b.WriteString(s.header())
		if s.Excerpt != "" {
			b.WriteString("\n")
			b.WriteString(s.Excerpt)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// header is the one line that identifies a source to the model, and the only
// place its label is spelled.
//
// The retrieval score is deliberately not in it. It used to be -- "(chunk 3,
// similarity 0.61)" -- on the theory that it told the model how strong a match
// was, and llama3.2:3b repeated it to the user ("with a similarity of 0.79"),
// which is an internal number presented as part of the answer. The score stays
// on the Source, where the client shows it as metadata; the model gets the
// sources in relevance order, which is the part of the score it can use.
func (s Source) header() string {
	head := "[" + s.Label + "] " + s.Type + ": " + strconv.Quote(s.Title)
	if s.ChunkIndex != nil {
		head += fmt.Sprintf(" (chunk %d)", *s.ChunkIndex)
	}
	return head
}

// label numbers a source. Labels are assigned in the order sources are
// retrieved -- documents, then memories, then tasks, goals and notes -- so S1
// is the strongest document match whenever any document matched at all.
func label(i int) string { return "S" + strconv.Itoa(i+1) }

// bracketed finds every square-bracketed group short enough to be a citation.
// A code block or a Markdown link can contain brackets too, which is why the
// label pattern is applied inside the group rather than to the whole answer.
var bracketed = regexp.MustCompile(`\[[^\][\n]{1,120}\]`)

// labelPattern matches S1 … S999 as a whole token.
var labelPattern = regexp.MustCompile(`\bS([1-9][0-9]{0,2})\b`)

// markCited sets Cited on every source the answer actually referenced.
//
// This is what makes the recorded sources honest. Retrieval hands the model
// five things; an answer typically uses one. Recording all five as "used"
// would make the stored citation trail useless for the exact question it
// exists to answer -- what was this claim based on?
func markCited(sources []Source, answer string) []Source {
	if len(sources) == 0 {
		return nil
	}
	byLabel := make(map[string]int, len(sources))
	for i, s := range sources {
		byLabel[s.Label] = i
	}
	for _, group := range bracketed.FindAllString(answer, -1) {
		for _, m := range labelPattern.FindAllString(group, -1) {
			if i, ok := byLabel[m]; ok {
				sources[i].Cited = true
			}
		}
	}
	return sources
}

// truncate cuts a string to at most max characters on a rune boundary, marking
// that it was cut. The model is told the excerpt is partial by the ellipsis
// rather than by a separate flag it might ignore.
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:max]), " \t\n") + "…"
}

// deriveTitle turns the first user message into a conversation title.
//
// A title is a label in a sidebar, so it is the first line collapsed to one
// line and cut short -- not a summary. Generating one with the model would
// cost a second round trip per conversation and is not worth it in this phase.
func deriveTitle(text string) string {
	const maxTitle = 60
	line := strings.TrimSpace(text)
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	return truncate(line, maxTitle)
}
