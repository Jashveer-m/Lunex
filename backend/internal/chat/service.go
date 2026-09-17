package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/actions"
	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
	"github.com/jashveer/lifeos/backend/internal/tools"
)

// Store is the slice of the repository the service needs. As everywhere else
// in this codebase, every method takes the owner id: a conversation cannot be
// reached without saying whose it is.
type Store interface {
	CreateConversation(ctx context.Context, userID uuid.UUID, title string) (Conversation, error)
	ConversationByID(ctx context.Context, userID, id uuid.UUID) (Conversation, error)
	ListConversations(ctx context.Context, userID uuid.UUID, f Filter) ([]Conversation, error)
	DeleteConversation(ctx context.Context, userID, id uuid.UUID) error
	// Messages returns the most recent `limit` messages of a conversation, in
	// chronological order.
	Messages(ctx context.Context, userID, convID uuid.UUID, limit int) ([]Message, error)
	// AppendTurn writes a whole turn atomically and names the conversation if
	// it is still using the default title. One transaction, because a stored
	// question with no answer -- or an answer with no question -- is not a
	// state any reader should have to handle.
	//
	// The turn's actions are part of it: a read the turn ran, or a write it
	// proposed, is written in the same transaction as the messages. So a turn
	// that fails leaves no proposal behind for the user to approve, and a
	// proposal is never announced before the row it names exists.
	AppendTurn(ctx context.Context, userID, convID uuid.UUID, msgs []NewMessage, titleIfDefault string, acts []actions.NewAction) ([]Message, []actions.Action, error)
}

// Sink receives a turn as it is produced. Every method is called from
// SendMessage's own goroutine, in order, and an error from any of them abandons
// the turn -- which is how a client that hung up stops the work it started.
type Sink interface {
	// Sources is called once, after the model has accepted the request and
	// before the first token, with everything retrieved for this question.
	Sources(sources []Source) error
	// Token is called for each fragment of the reply, in order. Concatenating
	// them gives exactly the stored message content.
	Token(text string) error
	// Actions is called at most once, after the turn is persisted and before
	// the extractors run, with the actions the turn produced -- so a proposal
	// reaches the client as soon as it is approvable, not a minute later when
	// the extractions finish. Its error is logged, not returned: by then the
	// turn is saved.
	Actions(acts []TurnAction) error
}

// Options are the generation knobs, resolved from configuration at boot.
type Options struct {
	// Model overrides the provider's own model for this service.
	Model string
	// Temperature is kept low by default: this assistant is quoting the user's
	// own notes back at them, and creative rephrasing of a deadline is a bug.
	Temperature float64
	MaxTokens   int
	// MinSimilarity is the document retrieval floor; see DefaultMinSimilarity.
	MinSimilarity float64
	// MemoryMinSimilarity is the floor for memory retrieval. It is a separate
	// number because it is a different question: see
	// memories.DefaultMinSimilarity for why it is higher.
	MemoryMinSimilarity float64
}

// Deps are everything the orchestrator needs.
//
// Calendar is optional on the same terms as the four below: a nil one is an
// assistant that retrieves what Phase 7 did, with no calendar in its context.
//
// Four of them are optional and independently so: a nil Memories is an
// assistant that retrieves exactly what Phase 4 did, a nil MemoryExtractor is
// one that uses what it already knows without learning anything new, and Graph
// and GraphExtractor are the same pair for Phase 6. None is a degraded mode to
// hide -- they are the halves of two systems, and an operator gets to run any
// combination of them.
//
// Phase 7's three -- Router, Tools, Actions -- are optional together: all
// three wired is an assistant that can use tools, and anything less is one
// that cannot, which is exactly Phase 6. Agent is the agent the router decides
// for; zero means agents.General.
type Deps struct {
	Store           Store
	Provider        ai.Provider
	Documents       DocumentSearcher
	Memories        MemorySearcher
	MemoryExtractor MemoryExtractor
	Graph           GraphSearcher
	GraphExtractor  GraphExtractor
	Tasks           TaskLister
	Goals           GoalLister
	Notes           NoteLister
	Calendar        EventLister
	Router          ToolRouter
	Tools           ToolRunner
	Actions         ActionLog
	Agent           agents.Agent
	Logger          *slog.Logger
	Options         Options
}

// Service holds the chat use cases. It is transport agnostic: SendMessage
// streams through a Sink, not through an http.ResponseWriter, so the
// orchestrator is testable without HTTP and reusable from a future job.
type Service struct {
	store     Store
	provider  ai.Provider
	docs      DocumentSearcher
	memories  MemorySearcher
	extractor MemoryExtractor
	graph     GraphSearcher
	linker    GraphExtractor
	tasks     TaskLister
	goals     GoalLister
	notes     NoteLister
	calendar  EventLister
	router    ToolRouter
	tools     ToolRunner
	actions   ActionLog
	agent     agents.Agent
	log       *slog.Logger
	opts      Options
	// now is injectable so the prompt's "current date" is assertable.
	now func() time.Time
}

func NewService(d Deps) *Service {
	opts := d.Options
	if opts.MinSimilarity == 0 {
		opts.MinSimilarity = DefaultMinSimilarity
	}
	if opts.MemoryMinSimilarity == 0 {
		opts.MemoryMinSimilarity = memories.DefaultMinSimilarity
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	agent := d.Agent
	if agent.Name == "" {
		agent = agents.General
	}
	return &Service{
		store: d.Store, provider: d.Provider,
		docs: d.Documents, memories: d.Memories, extractor: d.MemoryExtractor,
		graph: d.Graph, linker: d.GraphExtractor,
		tasks: d.Tasks, goals: d.Goals, notes: d.Notes, calendar: d.Calendar,
		router: d.Router, tools: d.Tools, actions: d.Actions, agent: agent,
		log: log, opts: opts, now: time.Now,
	}
}

// --- conversation CRUD ------------------------------------------------------

func (s *Service) Create(ctx context.Context, userID uuid.UUID, title string) (Conversation, error) {
	title, err := ValidateTitle(title)
	if err != nil {
		return Conversation{}, err
	}
	return s.store.CreateConversation(ctx, userID, title)
}

// Get returns a conversation with its messages, oldest first.
func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Conversation, error) {
	conv, err := s.store.ConversationByID(ctx, userID, id)
	if err != nil {
		return Conversation{}, err
	}
	msgs, err := s.store.Messages(ctx, userID, id, MaxMessagesPerRead)
	if err != nil {
		return Conversation{}, err
	}
	conv.Messages = msgs
	return conv, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Conversation, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.ListConversations(ctx, userID, f)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.DeleteConversation(ctx, userID, id)
}

// --- the orchestrator -------------------------------------------------------

// Turn is one question and its answer, as persisted.
type Turn struct {
	User      Message
	Assistant Message
	// Model names what generated the answer. Two models' answers are not
	// interchangeable, so which one produced this one is worth reporting.
	Model string
	// Remembered is what the memory extractor took from this exchange, which is
	// nil for most turns. It is reported rather than kept quiet: an assistant
	// that writes down facts about a user without telling them is the version
	// of this feature nobody asked for.
	Remembered []memories.Memory
	// Linked is what the relationship extractor took from it, on the same
	// terms and for the same reason.
	Linked []graph.Edge
	// Actions is what the turn did with a tool: a read it ran, or a write it
	// proposed and that now waits for the user. Nil on most turns.
	Actions []TurnAction
}

// SendMessage runs one turn: retrieve, prompt, generate, persist.
//
// The order of the first three steps is not arbitrary. Retrieval and the
// provider call both happen *before* the sink is touched, so everything that
// can fail with a meaningful status code -- an unknown conversation, an
// invalid message, an embedding or model outage -- fails while the HTTP layer
// can still choose one. Once the first token is on the wire the response is
// committed, and only a mid-generation failure has to be reported in-band.
//
// Persistence happens last and as one transaction. A turn whose generation
// failed is not stored at all: the alternative, keeping the question and
// discarding the answer, leaves a dangling user message that the next turn
// replays as history and that a client cannot distinguish from an unanswered
// question. Resending is the retry.
func (s *Service) SendMessage(ctx context.Context, userID, convID uuid.UUID, content string, sink Sink) (Turn, error) {
	content, err := ValidateMessage(content)
	if err != nil {
		return Turn{}, err
	}
	if sink == nil {
		sink = discardSink{}
	}

	// Ownership first, so a foreign conversation costs nothing but a lookup
	// and never reaches the model.
	if _, err := s.store.ConversationByID(ctx, userID, convID); err != nil {
		return Turn{}, err
	}

	// The tool step runs before retrieval, because a read tool's results are
	// retrieved sources too -- the most targeted ones the turn has -- and they
	// go first.
	step, err := s.act(ctx, userID, convID, content)
	if err != nil {
		return Turn{}, err
	}

	sources, err := s.retrieve(ctx, userID, content, step.sources)
	if err != nil {
		return Turn{}, err
	}

	history, err := s.store.Messages(ctx, userID, convID, MaxHistoryMessages)
	if err != nil {
		return Turn{}, fmt.Errorf("load history: %w", err)
	}

	prompt := buildPrompt(s.now(), s.toolsEnabled(), sources, step.actionsBlock(sources), history, content)
	stream, err := s.provider.Chat(ctx, prompt, ai.Options{
		Model: s.opts.Model, Temperature: s.opts.Temperature, MaxTokens: s.opts.MaxTokens,
	})
	if err != nil {
		return Turn{}, fmt.Errorf("chat: %w", err)
	}
	defer stream.Close() //nolint:errcheck // releases the upstream connection

	if err := sink.Sources(sources); err != nil {
		return Turn{}, err
	}

	var answer strings.Builder
	for stream.Next() {
		piece := stream.Text()
		answer.WriteString(piece)
		if err := sink.Token(piece); err != nil {
			return Turn{}, err
		}
	}
	if err := stream.Err(); err != nil {
		return Turn{}, fmt.Errorf("chat: %w", err)
	}

	text := strings.TrimSpace(answer.String())
	if text == "" {
		// An empty reply is a failed generation, not an answer. Storing it
		// would put a blank assistant turn in the history the next question
		// replays.
		return Turn{}, fmt.Errorf("%w: the model returned an empty reply", ai.ErrUnavailable)
	}

	// Which of the retrieved sources the answer actually referenced. Measured
	// from the generated text, never assumed.
	sources = markCited(sources, text)

	written, recorded, err := s.store.AppendTurn(ctx, userID, convID, []NewMessage{
		{Role: RoleUser, Content: content},
		{Role: RoleAssistant, Content: text, Sources: sources},
	}, deriveTitle(content), step.newActions())
	if err != nil {
		return Turn{}, fmt.Errorf("persist turn: %w", err)
	}
	if len(written) != 2 {
		return Turn{}, fmt.Errorf("persist turn: wrote %d messages, want 2", len(written))
	}

	turnActions := step.turnActions(recorded)
	if len(turnActions) > 0 {
		if err := sink.Actions(turnActions); err != nil {
			// The turn is saved and the proposal with it; a client that hung up
			// finds it at GET /actions. Not a failure to report.
			s.log.Info("could not announce the turn's actions", "error", err,
				"user_id", userID, "conversation_id", convID)
		}
	}

	s.log.Info("chat turn",
		"user_id", userID, "conversation_id", convID, "model", s.model(),
		"sources_retrieved", len(sources), "sources_cited", countCited(sources),
		"answer_chars", len(text), "tool", step.toolName(), "actions", len(turnActions))

	// What the extractors must not read as fact: the changes this conversation
	// asked for that have not happened, and the documents the answer drew on.
	unconfirmed := step.unconfirmed()
	return Turn{
		User: written[0], Assistant: written[1], Model: s.model(),
		Actions: turnActions,
		Remembered: s.remember(ctx, userID, convID, memories.Turn{
			UserMessage: content, AssistantMessage: text,
			Unconfirmed: unconfirmed, Retrieved: documentText(sources),
		}),
		Linked: s.link(ctx, userID, convID, graph.Turn{
			UserMessage: content, AssistantMessage: text, Unconfirmed: unconfirmed,
		}),
	}, nil
}

// documentText is the text of every document passage among the sources,
// whether retrieval or a read tool found it.
func documentText(sources []Source) []string {
	var out []string
	for _, s := range sources {
		if s.Type == SourceDocument && s.Excerpt != "" {
			out = append(out, s.Excerpt)
		}
	}
	return out
}

// remember hands the finished exchange to the memory extractor.
//
// Three things about where this sits are deliberate.
//
// It runs *after* the turn is persisted and after the last token has been
// streamed. Extraction is a second model call -- tens of seconds on a local 3B
// model -- and the user should not wait for it to see their answer. What they
// do wait for is the `done` frame, which is sent once this returns: the answer
// is complete and readable on screen throughout, and the cost of the memory
// system is a delayed end-of-stream marker rather than a delayed reply. Moving
// it off the request would be a background job, which this phase does not have.
//
// It cannot fail the turn. A failure to remember something is logged and
// dropped: the answer is already generated, already stored and already on the
// user's screen, and turning that into a 500 because a second model call timed
// out would be an outright regression.
//
// And it is given the same context as the turn, so a client that hangs up
// cancels it. The exchange is already saved; extracting facts for a request
// nobody is listening to can wait for the next one.
func (s *Service) remember(ctx context.Context, userID, convID uuid.UUID, turn memories.Turn) []memories.Memory {
	if s.extractor == nil {
		return nil
	}
	stored, err := s.extractor.Extract(ctx, userID, convID, turn)
	if err != nil {
		s.log.Warn("memory extraction failed",
			"error", err, "user_id", userID, "conversation_id", convID,
			"stored_before_failure", len(stored))
	}
	return stored
}

// link hands the finished exchange to the relationship extractor, on exactly
// the terms remember does: after the turn is persisted and streamed, on the
// turn's own context, and unable to fail it.
//
// It runs *after* the memory extraction rather than beside it, and that is a
// decision rather than an omission. Both calls go to the same Ollama, which
// holds one model resident and serves requests to it in sequence: running them
// concurrently would not halve the wait, it would interleave two queue entries
// and make the turn's tail latency harder to reason about for no gain. On a
// deployment with two model servers -- or one hosted provider -- that
// arithmetic changes, and this is the function that would change with it.
func (s *Service) link(ctx context.Context, userID, convID uuid.UUID, turn graph.Turn) []graph.Edge {
	if s.linker == nil {
		return nil
	}
	stored, err := s.linker.Extract(ctx, userID, convID, turn)
	if err != nil {
		s.log.Warn("relationship extraction failed",
			"error", err, "user_id", userID, "conversation_id", convID,
			"stored_before_failure", len(stored))
	}
	return stored
}

// ActionExecuted is the action engine's ExecutedHook: an approved change to
// the user's data has just been made, so the relationships the turn that
// proposed it could not record are read now, against the record it produced.
//
// When the turn happened, the change was only a proposal, and link dropped
// every relationship with an end named in it rather than invent a node for a
// task that did not exist. This is the other half: the same user message, an
// answer that says what was actually done, and an anchor that makes an end
// naming the record resolve to the record's own node. Approving "Renew my
// passport" now connects the task node the approval created, rather than
// leaving it isolated beside an extracted "passport renewal" project.
//
// Memories get no second pass. The change is recorded as the task, goal or
// note itself, which retrieval already reads; a memory restating it would be a
// second copy that does not follow the record when it is edited or deleted.
//
// It runs in the background after the approval has answered, and it can fail
// only into the log.
func (s *Service) ActionExecuted(ctx context.Context, userID uuid.UUID, a actions.Action, summary string, result tools.Result) {
	if s.linker == nil || a.ConversationID == nil {
		return
	}
	anchor := anchorFor(result)
	if anchor == nil {
		return
	}
	convID := *a.ConversationID
	question, ok, err := s.proposingMessage(ctx, userID, convID, a)
	if err != nil {
		s.log.Warn("relationship extraction after approval: could not load the conversation",
			"error", err, "user_id", userID, "action_id", a.ID)
		return
	}
	if !ok {
		s.log.Debug("relationship extraction after approval skipped: the proposing message is gone",
			"user_id", userID, "action_id", a.ID)
		return
	}
	s.link(ctx, userID, convID, graph.Turn{
		UserMessage:      question,
		AssistantMessage: "The user approved this change and it has been made: " + summary,
		Anchor:           anchor,
	})
}

// anchorFor is the record an executed write produced. Every write tool creates
// or changes exactly one.
func anchorFor(r tools.Result) *graph.Anchor {
	switch {
	case len(r.Tasks) > 0:
		return &graph.Anchor{RefTable: "tasks", RefID: r.Tasks[0].ID, Label: r.Tasks[0].Title}
	case len(r.Goals) > 0:
		return &graph.Anchor{RefTable: "goals", RefID: r.Goals[0].ID, Label: r.Goals[0].Title}
	case len(r.Notes) > 0:
		return &graph.Anchor{RefTable: "notes", RefID: r.Notes[0].ID, Label: r.Notes[0].Title}
	case len(r.Events) > 0:
		return &graph.Anchor{RefTable: "calendar_events", RefID: r.Events[0].ID, Label: r.Events[0].Title}
	}
	return nil
}

// proposingMessage finds the user message whose turn proposed an action.
//
// The action is inserted in the same transaction as the turn's messages, after
// them, and every one of those rows takes clock_timestamp() -- so the proposing
// message is the last user message at or before the action's timestamp. The
// next turn's message is always after it.
func (s *Service) proposingMessage(ctx context.Context, userID, convID uuid.UUID, a actions.Action) (string, bool, error) {
	msgs, err := s.store.Messages(ctx, userID, convID, MaxMessagesPerRead)
	if err != nil {
		return "", false, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == RoleUser && !m.CreatedAt.After(a.CreatedAt) {
			return m.Content, true, nil
		}
	}
	return "", false, nil
}

// model names what actually generated the answer: the per-service override if
// one is configured, otherwise the provider's own.
func (s *Service) model() string {
	if s.opts.Model != "" {
		return s.opts.Model
	}
	return s.provider.Model()
}

func countCited(sources []Source) int {
	n := 0
	for _, s := range sources {
		if s.Cited {
			n++
		}
	}
	return n
}

// discardSink is what a caller that only wants the finished turn gets.
type discardSink struct{}

func (discardSink) Sources([]Source) error     { return nil }
func (discardSink) Token(string) error         { return nil }
func (discardSink) Actions([]TurnAction) error { return nil }

// CollectSink accumulates a turn in memory. It is the non-streaming client:
// used by tests, and by anything later that wants the answer as a value.
type CollectSink struct {
	Retrieved []Source
	Text      strings.Builder
	Announced []TurnAction
	// TokensBeforeActions is how many tokens had arrived when the actions were
	// announced, which is how a test pins that a proposal is announced after
	// the answer rather than before its row exists.
	TokensBeforeActions int
	tokens              int
}

func (c *CollectSink) Sources(s []Source) error { c.Retrieved = s; return nil }
func (c *CollectSink) Token(t string) error     { c.Text.WriteString(t); c.tokens++; return nil }
func (c *CollectSink) Actions(a []TurnAction) error {
	c.Announced, c.TokensBeforeActions = a, c.tokens
	return nil
}

// Unavailable reports whether an error means the turn could not run because
// something upstream was down -- the model, or the embedding service retrieval
// needs -- which is a 503 a client should retry on rather than a 500.
func Unavailable(err error) bool {
	return errors.Is(err, ai.ErrUnavailable) || errors.Is(err, documents.ErrEmbedding)
}
