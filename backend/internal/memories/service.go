package memories

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. Every method takes
// the owner id, so a memory cannot be reached without saying whose it is.
type Store interface {
	Create(ctx context.Context, userID uuid.UUID, in CreateInput) (Memory, error)
	ByID(ctx context.Context, userID, id uuid.UUID) (Memory, error)
	List(ctx context.Context, userID uuid.UUID, f Filter) ([]Memory, error)
	Update(ctx context.Context, userID, id uuid.UUID, p Patch, embedding []float32) (Memory, error)
	Delete(ctx context.Context, userID, id uuid.UUID) error
	DeleteAll(ctx context.Context, userID uuid.UUID) (int, error)
	Search(ctx context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error)
}

// Options are the extraction knobs, resolved from configuration at boot.
type Options struct {
	// Model overrides the provider's own model for extraction. A smaller,
	// faster model than the one answering the user is a reasonable choice here
	// -- this call classifies, it does not converse.
	Model string
	// Temperature is near-zero by default: extraction is a reading task, and a
	// creatively rephrased "fact" about the user is a fabrication.
	Temperature float64
	// MaxTokens caps the extraction reply. Three one-sentence facts as JSON is
	// a few hundred tokens; the cap is what stops a model that decided to
	// explain itself from generating for a minute.
	MaxTokens int
	// Timeout bounds the whole extraction. It is the number that decides how
	// much latency memory adds to a chat turn; see ExtractFromTurn.
	Timeout time.Duration
	// JSONMode asks the provider to constrain decoding to well-formed JSON.
	//
	// It is off by default, and that is a decision taken from measurement.
	// Ollama's JSON mode on llama3.2:3b makes extraction strictly worse: the
	// model satisfies the constraint immediately with `{}` and then pads
	// whitespace until it hits the token limit, so the stream ends without a
	// done frame and the extraction is reported as a truncated generation. The
	// same prompt without the constraint returns a clean JSON array, or a clean
	// `[]`. Constrained decoding is a real capability and a bigger model uses it
	// well, which is why the switch exists rather than the code -- but the
	// default has to be what works with the model this phase ships with. See
	// docs/decisions.md.
	JSONMode bool
}

// Extraction defaults, applied to a zero Options.
const (
	DefaultExtractionTemperature = 0.1
	DefaultExtractionMaxTokens   = 512
	DefaultExtractionTimeout     = 60 * time.Second
)

// Deps are everything the service needs.
type Deps struct {
	Store Store
	// Provider is the model that reads a turn and proposes facts. It is the
	// same interface the chat orchestrator uses, so extraction can be pointed
	// at a different model without either package learning about the other.
	Provider ai.Provider
	Embedder embeddings.Embedder
	Logger   *slog.Logger
	Options  Options
}

// Service holds the memory use cases. It is transport agnostic: Search and
// ExtractFromTurn are called by the chat orchestrator as plain functions, with
// no HTTP in the way.
type Service struct {
	store    Store
	provider ai.Provider
	embedder embeddings.Embedder
	log      *slog.Logger
	opts     Options
}

func NewService(d Deps) *Service {
	opts := d.Options
	if opts.Temperature == 0 {
		opts.Temperature = DefaultExtractionTemperature
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultExtractionMaxTokens
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultExtractionTimeout
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: d.Store, provider: d.Provider, embedder: d.Embedder, log: log, opts: opts}
}

// --- the extraction pipeline ------------------------------------------------

// ExtractFromTurn reads one completed exchange and stores what is worth
// keeping. It returns the memories it wrote, which is nil far more often than
// not -- most turns contain nothing durable, and that is the correct outcome.
//
// It is called by the chat orchestrator after the answer has been streamed and
// the turn persisted, and its error is logged rather than returned to the user:
// a failure to remember something must never turn a good answer into a 500. See
// chat.Service.SendMessage.
//
// The cost is a second model call per substantive turn. Locally that is free in
// money and real in time -- tens of seconds on a 3B model -- which is why
// WorthExtracting gates it, why Options.Timeout bounds it, and why it runs
// after the last token rather than before the first.
func (s *Service) ExtractFromTurn(ctx context.Context, userID, conversationID uuid.UUID, userMessage, assistantMessage string) ([]Memory, error) {
	if !WorthExtracting(userMessage, assistantMessage) {
		s.log.Debug("memory extraction skipped: turn is too short",
			"user_id", userID, "conversation_id", conversationID)
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	candidates, err := s.propose(ctx, userMessage, assistantMessage)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	texts := make([]string, len(candidates))
	for i, c := range candidates {
		texts[i] = c.Content
	}
	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("%w: got %d vectors for %d facts", ErrEmbedding, len(vectors), len(texts))
	}

	source := conversationID
	stored := make([]Memory, 0, len(candidates))
	for i, c := range candidates {
		duplicate, err := s.isDuplicate(ctx, userID, vectors[i])
		if err != nil {
			return stored, err
		}
		if duplicate {
			s.log.Debug("memory already known", "user_id", userID, "content", c.Content)
			continue
		}
		m, err := s.store.Create(ctx, userID, CreateInput{
			Candidate: c, SourceConversationID: &source, Embedding: vectors[i],
		})
		if err != nil {
			// Whatever was written before this point stays written: the facts
			// are independent, and rolling back a stored one because the next
			// insert failed would lose it for no gain.
			return stored, err
		}
		stored = append(stored, m)
	}

	if len(stored) > 0 {
		s.log.Info("memories extracted",
			"user_id", userID, "conversation_id", conversationID,
			"proposed", len(candidates), "stored", len(stored), "facts", summarize(candidates))
	}
	return stored, nil
}

// propose runs the model and turns its reply into candidate facts.
func (s *Service) propose(ctx context.Context, userMessage, assistantMessage string) ([]Candidate, error) {
	format := ""
	if s.opts.JSONMode {
		format = ai.FormatJSON
	}
	stream, err := s.provider.Chat(ctx, ExtractionPrompt(userMessage, assistantMessage), ai.Options{
		Model:       s.opts.Model,
		Temperature: s.opts.Temperature,
		MaxTokens:   s.opts.MaxTokens,
		// Off by default; see Options.JSONMode. Either way the reply is parsed
		// defensively, because JSON asked for and JSON guaranteed are different
		// things and ParseExtraction cannot tell which it is looking at.
		Format: format,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExtraction, err)
	}
	defer stream.Close() //nolint:errcheck // releases the upstream connection

	reply, err := ai.Collect(stream)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExtraction, err)
	}

	candidates := ParseExtraction(reply)
	if len(candidates) == 0 {
		return nil, nil
	}

	kept := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Confidence < MinKeptConfidence || c.Importance < MinKeptImportance {
			s.log.Debug("memory dropped: the model scored it too low",
				"importance", c.Importance, "confidence", c.Confidence, "content", c.Content)
			continue
		}
		if !AboutTheUser(c.Content) {
			s.log.Debug("memory dropped: not a fact about the user", "content", c.Content)
			continue
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// isDuplicate reports whether the user already has essentially this fact.
//
// The check is a similarity search rather than a text comparison because the
// same fact is restated in different words -- which is exactly the case that
// would otherwise fill the table with near-copies of one preference.
func (s *Service) isDuplicate(ctx context.Context, userID uuid.UUID, embedding []float32) (bool, error) {
	near, err := s.store.Search(ctx, userID, embedding, SearchQuery{
		Limit: 1, MinSimilarity: DuplicateSimilarity,
	})
	if err != nil {
		return false, fmt.Errorf("check for a duplicate memory: %w", err)
	}
	return len(near) > 0, nil
}

// --- retrieval --------------------------------------------------------------

// Search embeds the query and returns the nearest owner-scoped memories.
//
// This is the function the chat orchestrator calls on every turn. It takes the
// user id as an argument rather than reading it from a request context, so it
// is usable from an agent loop, a job or a test with no HTTP anywhere.
func (s *Service) Search(ctx context.Context, userID uuid.UUID, q SearchQuery) ([]SearchResult, error) {
	q, err := ValidateSearch(q)
	if err != nil {
		return nil, err
	}

	vectors, err := s.embedder.Embed(ctx, []string{q.Query})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("%w: got %d vectors for the query", ErrEmbedding, len(vectors))
	}

	return s.store.Search(ctx, userID, vectors[0], q)
}

// --- management -------------------------------------------------------------

func (s *Service) Get(ctx context.Context, userID, id uuid.UUID) (Memory, error) {
	return s.store.ByID(ctx, userID, id)
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, f Filter) ([]Memory, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.List(ctx, userID, f)
}

// Update edits a memory's text or switches it on and off.
//
// Changed content is re-embedded before the write. The two have to move
// together: a memory whose text says one thing and whose vector still points at
// the old wording is retrievable by what it used to say and invisible to what
// it now says, which is worse than either.
func (s *Service) Update(ctx context.Context, userID, id uuid.UUID, in UpdateInput) (Memory, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Memory{}, err
	}

	var embedding []float32
	if p.Content != nil {
		vectors, err := s.embedder.Embed(ctx, []string{*p.Content})
		if err != nil {
			return Memory{}, fmt.Errorf("%w: %w", ErrEmbedding, err)
		}
		if len(vectors) != 1 {
			return Memory{}, fmt.Errorf("%w: got %d vectors for the edited memory", ErrEmbedding, len(vectors))
		}
		embedding = vectors[0]
	}

	return s.store.Update(ctx, userID, id, p, embedding)
}

func (s *Service) Delete(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.Delete(ctx, userID, id)
}

// Clear deletes everything the assistant has remembered about one user.
//
// The confirmation flag is checked here rather than only in the handler,
// because "forget everything about me" is irreversible in this phase -- there
// is no archive and no undo -- and the service is the layer every caller,
// including a future agent, goes through.
func (s *Service) Clear(ctx context.Context, userID uuid.UUID, confirm bool) (int, error) {
	if !confirm {
		return 0, validate.Errors{{
			Field:   "confirm",
			Message: "must be true to delete every memory",
		}}
	}
	n, err := s.store.DeleteAll(ctx, userID)
	if err != nil {
		return 0, err
	}
	s.log.Info("memories cleared", "user_id", userID, "deleted", n)
	return n, nil
}
