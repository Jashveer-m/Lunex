package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
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
	AppendTurn(ctx context.Context, userID, convID uuid.UUID, msgs []NewMessage, titleIfDefault string) ([]Message, error)
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
}

// Options are the generation knobs, resolved from configuration at boot.
type Options struct {
	// Model overrides the provider's own model for this service.
	Model string
	// Temperature is kept low by default: this assistant is quoting the user's
	// own notes back at them, and creative rephrasing of a deadline is a bug.
	Temperature float64
	MaxTokens   int
	// MinSimilarity is the retrieval floor; see DefaultMinSimilarity.
	MinSimilarity float64
}

// Deps are everything the orchestrator needs.
type Deps struct {
	Store     Store
	Provider  ai.Provider
	Documents DocumentSearcher
	Tasks     TaskLister
	Goals     GoalLister
	Notes     NoteLister
	Logger    *slog.Logger
	Options   Options
}

// Service holds the chat use cases. It is transport agnostic: SendMessage
// streams through a Sink, not through an http.ResponseWriter, so the
// orchestrator is testable without HTTP and reusable from a future job.
type Service struct {
	store    Store
	provider ai.Provider
	docs     DocumentSearcher
	tasks    TaskLister
	goals    GoalLister
	notes    NoteLister
	log      *slog.Logger
	opts     Options
	// now is injectable so the prompt's "current date" is assertable.
	now func() time.Time
}

func NewService(d Deps) *Service {
	opts := d.Options
	if opts.MinSimilarity == 0 {
		opts.MinSimilarity = DefaultMinSimilarity
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: d.Store, provider: d.Provider,
		docs: d.Documents, tasks: d.Tasks, goals: d.Goals, notes: d.Notes,
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

	sources, err := s.retrieve(ctx, userID, content)
	if err != nil {
		return Turn{}, err
	}

	history, err := s.store.Messages(ctx, userID, convID, MaxHistoryMessages)
	if err != nil {
		return Turn{}, fmt.Errorf("load history: %w", err)
	}

	prompt := buildPrompt(s.now(), sources, history, content)
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

	written, err := s.store.AppendTurn(ctx, userID, convID, []NewMessage{
		{Role: RoleUser, Content: content},
		{Role: RoleAssistant, Content: text, Sources: sources},
	}, deriveTitle(content))
	if err != nil {
		return Turn{}, fmt.Errorf("persist turn: %w", err)
	}
	if len(written) != 2 {
		return Turn{}, fmt.Errorf("persist turn: wrote %d messages, want 2", len(written))
	}

	s.log.Info("chat turn",
		"user_id", userID, "conversation_id", convID, "model", s.model(),
		"sources_retrieved", len(sources), "sources_cited", countCited(sources),
		"answer_chars", len(text))

	return Turn{User: written[0], Assistant: written[1], Model: s.model()}, nil
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

func (discardSink) Sources([]Source) error { return nil }
func (discardSink) Token(string) error     { return nil }

// CollectSink accumulates a turn in memory. It is the non-streaming client:
// used by tests, and by anything later that wants the answer as a value.
type CollectSink struct {
	Retrieved []Source
	Text      strings.Builder
}

func (c *CollectSink) Sources(s []Source) error { c.Retrieved = s; return nil }
func (c *CollectSink) Token(t string) error     { c.Text.WriteString(t); return nil }

// Unavailable reports whether an error means the turn could not run because
// something upstream was down -- the model, or the embedding service retrieval
// needs -- which is a 503 a client should retry on rather than a 500.
func Unavailable(err error) bool {
	return errors.Is(err, ai.ErrUnavailable) || errors.Is(err, documents.ErrEmbedding)
}
