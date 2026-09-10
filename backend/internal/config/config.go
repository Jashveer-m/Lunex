// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jashveer/lifeos/backend/internal/agents"
	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/chat"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/embeddings"
	"github.com/jashveer/lifeos/backend/internal/graph"
	"github.com/jashveer/lifeos/backend/internal/memories"
)

type Config struct {
	Port            string
	DatabaseURL     string
	JWTSecret       []byte
	JWTIssuer       string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// LoginRateLimit is the per-IP request budget for the auth endpoints.
	LoginRateLimit      float64
	LoginRateLimitBurst int

	// Phase 3: documents and retrieval.
	OllamaBaseURL       string
	EmbeddingModel      string
	EmbeddingDimensions int
	MaxUploadBytes      int64
	// DocumentProcessTimeout bounds one synchronous upload: extract, chunk,
	// embed and store. It also sets the server's write timeout, because a
	// response the server has already given up on cannot report the result.
	DocumentProcessTimeout time.Duration

	// Phase 4: the AI assistant.
	ChatModel string
	// ChatTimeout bounds one whole turn: retrieval, generation, the write --
	// *and* the memory and relationship extractions, which run on the turn's
	// own context after it.
	//
	// A local 3B model answering from five retrieved chunks is tens of seconds
	// of honest work, so it is far larger than the 30s the rest of the API
	// gets, and, like the document timeout, it also has to fit inside the
	// server's write timeout or the stream would be cut at the socket.
	//
	// The default has grown by one minute for each model call a phase added to
	// the turn, and that is not a round number picked for comfort. Phase 6
	// added a second 60s extraction to the same budget, and Phase 7 a 60s
	// routing call in front of the answer; keeping the answer's headroom means
	// adding the same minute each time -- and not adding it is a turn that
	// answers, persists, and *then* has its context cancelled out from under
	// the end-of-stream frame, reported to the user as a failed answer that was
	// in fact already saved. MaxExtractionShare below is the boot-time check.
	ChatTimeout     time.Duration
	ChatTemperature float64
	ChatMaxTokens   int
	// ChatMinSimilarity is the retrieval floor for chat. Below it a chunk is
	// not shown to the model at all, which is the main defence against an
	// unrelated question coming back with a confident citation.
	ChatMinSimilarity float64

	// Phase 5: the memory system.
	//
	// MemoryExtraction switches the *writing* half on and off. Retrieval is
	// always wired: an assistant that has memories should use them, and one
	// with none loses nothing by looking. Extraction is the half that costs a
	// second model call on every substantial turn, so it is the half with a
	// switch.
	MemoryExtraction bool
	// MemoryModel overrides the chat model for extraction. Empty means the
	// same model answers and extracts. A smaller one is a reasonable choice:
	// extraction reads and classifies, it does not converse.
	MemoryModel string
	// MemoryExtractTimeout bounds one extraction. It is the latency memory adds
	// to the end of a chat turn, so it is the knob to turn when `done` arrives
	// too late -- and it fits inside ChatTimeout, since extraction runs on the
	// turn's own context.
	MemoryExtractTimeout time.Duration
	MemoryMaxTokens      int
	MemoryTemperature    float64
	// MemoryJSONMode asks the provider to constrain extraction to well-formed
	// JSON. Off by default: it makes llama3.2:3b strictly worse. See
	// memories.Options.JSONMode.
	MemoryJSONMode bool
	// MemoryMinSimilarity is the retrieval floor for memories, separate from
	// the document floor and higher; see memories.DefaultMinSimilarity.
	MemoryMinSimilarity float64

	// Phase 6: the knowledge graph.
	//
	// GraphExtraction switches the *writing* half on and off, exactly as
	// MemoryExtraction does and for the same reason: it is the half that costs
	// a model call on every substantial turn. Node sync and the 1-hop lookup
	// are always wired -- syncing a node is one upsert on a write the user
	// already made, and an assistant that has a graph should use it.
	GraphExtraction bool
	// GraphModel overrides the chat model for relationship extraction. Empty
	// means the same model answers and extracts.
	GraphModel string
	// GraphExtractTimeout bounds one relationship extraction. It is the second
	// piece of latency the tail of a chat turn carries, after the memory
	// extraction it runs behind, and both have to fit inside ChatTimeout since
	// they run on the turn's own context.
	GraphExtractTimeout time.Duration
	GraphMaxTokens      int
	GraphTemperature    float64
	// GraphJSONMode asks the provider to constrain extraction to well-formed
	// JSON. Off by default, for the reason memories.Options.JSONMode records.
	GraphJSONMode bool

	// Phase 7: tools and the action engine.
	//
	// AgentTools switches the assistant's tools on and off as a whole: the
	// routing call, the read tools and write proposals. Off is exactly the
	// Phase 6 assistant, which says it cannot take actions. The approval
	// endpoints stay mounted either way, so a proposal made before the switch
	// was flipped can still be approved or rejected.
	AgentTools bool
	// AgentModel overrides the chat model for the routing call. Empty means
	// the same model answers and routes.
	AgentModel string
	// AgentTimeout bounds one routing decision. Unlike the extractions it is
	// spent *before* the first token -- it is the latency tools add to a turn
	// that looks like it asks for one -- and it comes out of the same
	// ChatTimeout budget.
	AgentTimeout     time.Duration
	AgentMaxTokens   int
	AgentTemperature float64
}

// DefaultChatTimeout is the budget for one whole turn; see Config.ChatTimeout
// for why it grew from three minutes to six.
const DefaultChatTimeout = 6 * time.Minute

// MaxExtractionShare is how much of ChatTimeout the model calls that are not
// the answer -- the two extractions after it and, since Phase 7, the routing
// call before it -- are allowed to occupy between them. The rest is what
// retrieval and generation have.
//
// The rule is proportional rather than an absolute floor because the absolute
// number is not knowable here: it is a property of the model, and an operator
// on a hosted one may reasonably run the whole turn in thirty seconds. "The
// answer gets at least as much of the budget as the bookkeeping after it" is
// true at every scale.
//
// It exists because the failure it prevents looks like something else
// entirely. The extractions run on the turn's own context, so an operator who
// raises MEMORY_EXTRACT_TIMEOUT and GRAPH_EXTRACT_TIMEOUT without touching
// CHAT_TIMEOUT does not get slower extraction -- they get turns that generate a
// good answer, persist it, and then have the context cancelled under the
// end-of-stream frame, which the client is shown as "the answer could not be
// completed, and nothing was saved". Both halves of that are false, and the
// answer is sitting in the database while the user reads it.
const MaxExtractionShare = 0.5

// WriteTimeout is how long the HTTP server will spend producing a response.
//
// Uploads process synchronously in this phase, so the server has to outlive
// the pipeline or a slow document would be cut off at the socket with the
// document row left mid-flight. The slack covers writing the response itself.
// When the job queue arrives this drops back to a flat 30s.
func (c Config) WriteTimeout() time.Duration {
	const base = 30 * time.Second
	// The longest thing a single request can legitimately do, plus slack for
	// writing the response. Chat streams for as long as the model generates,
	// so it counts here alongside an upload. The memory and graph extractions
	// run inside the chat turn's own budget, not after it, so they add nothing
	// here.
	longest := max(c.DocumentProcessTimeout, c.ChatTimeout)
	if d := longest + 15*time.Second; d > base {
		return d
	}
	return base
}

// Load reads configuration from the environment, applying defaults for
// everything except the values that must never have one.
func Load() (Config, error) {
	cfg := Config{
		Port:                envOr("PORT", "8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		JWTSecret:           []byte(os.Getenv("JWT_SECRET")),
		JWTIssuer:           envOr("JWT_ISSUER", "lifeos"),
		LoginRateLimitBurst: 10,
		LoginRateLimit:      0.2, // 1 request per 5s sustained, per IP

		OllamaBaseURL:       envOr("OLLAMA_BASE_URL", embeddings.DefaultBaseURL),
		EmbeddingModel:      envOr("EMBEDDING_MODEL", embeddings.DefaultModel),
		EmbeddingDimensions: embeddings.DefaultDimensions,
		MaxUploadBytes:      documents.MaxUploadBytes,

		ChatModel:         envOr("CHAT_MODEL", ai.DefaultModel),
		ChatTemperature:   0.2, // low: the assistant quotes the user's own data back
		ChatMaxTokens:     1024,
		ChatMinSimilarity: chat.DefaultMinSimilarity,

		MemoryExtraction:    true,
		MemoryModel:         os.Getenv("MEMORY_MODEL"),
		MemoryMaxTokens:     memories.DefaultExtractionMaxTokens,
		MemoryTemperature:   memories.DefaultExtractionTemperature,
		MemoryMinSimilarity: memories.DefaultMinSimilarity,

		GraphExtraction:  true,
		GraphModel:       os.Getenv("GRAPH_MODEL"),
		GraphMaxTokens:   graph.DefaultExtractionMaxTokens,
		GraphTemperature: graph.DefaultExtractionTemperature,

		AgentTools:       true,
		AgentModel:       os.Getenv("AGENT_MODEL"),
		AgentMaxTokens:   agents.DefaultMaxTokens,
		AgentTemperature: agents.DefaultTemperature,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	// A short or missing signing key silently weakens every access token, so
	// refuse to boot rather than degrade.
	if len(cfg.JWTSecret) < 32 {
		return Config{}, errors.New("JWT_SECRET is required and must be at least 32 bytes")
	}

	var err error
	if cfg.AccessTokenTTL, err = durationOr("ACCESS_TOKEN_TTL", 15*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.RefreshTokenTTL, err = durationOr("REFRESH_TOKEN_TTL", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if cfg.DocumentProcessTimeout, err = durationOr("DOCUMENT_PROCESS_TIMEOUT", 2*time.Minute); err != nil {
		return Config{}, err
	}
	if cfg.ChatTimeout, err = durationOr("CHAT_TIMEOUT", DefaultChatTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ChatTemperature, err = floatOr("CHAT_TEMPERATURE", cfg.ChatTemperature, 0, 2); err != nil {
		return Config{}, err
	}
	// The floor runs -1 … 1 because it is a cosine similarity, but a negative
	// floor admits everything and is never what an operator means.
	if cfg.ChatMinSimilarity, err = floatOr("CHAT_MIN_SIMILARITY", cfg.ChatMinSimilarity, 0, 1); err != nil {
		return Config{}, err
	}
	if cfg.MemoryExtractTimeout, err = durationOr("MEMORY_EXTRACT_TIMEOUT", memories.DefaultExtractionTimeout); err != nil {
		return Config{}, err
	}
	if cfg.MemoryTemperature, err = floatOr("MEMORY_TEMPERATURE", cfg.MemoryTemperature, 0, 2); err != nil {
		return Config{}, err
	}
	if cfg.MemoryMinSimilarity, err = floatOr("MEMORY_MIN_SIMILARITY", cfg.MemoryMinSimilarity, 0, 1); err != nil {
		return Config{}, err
	}
	if cfg.MemoryExtraction, err = boolOr("MEMORY_EXTRACTION", cfg.MemoryExtraction); err != nil {
		return Config{}, err
	}
	if cfg.MemoryJSONMode, err = boolOr("MEMORY_JSON_MODE", cfg.MemoryJSONMode); err != nil {
		return Config{}, err
	}
	if cfg.GraphExtractTimeout, err = durationOr("GRAPH_EXTRACT_TIMEOUT", graph.DefaultExtractionTimeout); err != nil {
		return Config{}, err
	}
	if cfg.GraphTemperature, err = floatOr("GRAPH_TEMPERATURE", cfg.GraphTemperature, 0, 2); err != nil {
		return Config{}, err
	}
	if cfg.GraphExtraction, err = boolOr("GRAPH_EXTRACTION", cfg.GraphExtraction); err != nil {
		return Config{}, err
	}
	if cfg.GraphJSONMode, err = boolOr("GRAPH_JSON_MODE", cfg.GraphJSONMode); err != nil {
		return Config{}, err
	}
	if cfg.AgentTools, err = boolOr("AGENT_TOOLS", cfg.AgentTools); err != nil {
		return Config{}, err
	}
	if cfg.AgentTimeout, err = durationOr("AGENT_TIMEOUT", agents.DefaultTimeout); err != nil {
		return Config{}, err
	}
	if cfg.AgentTemperature, err = floatOr("AGENT_TEMPERATURE", cfg.AgentTemperature, 0, 2); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("AGENT_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("AGENT_MAX_TOKENS: want a positive integer, got %q", v)
		}
		cfg.AgentMaxTokens = n
	}
	if v := os.Getenv("GRAPH_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("GRAPH_MAX_TOKENS: want a positive integer, got %q", v)
		}
		cfg.GraphMaxTokens = n
	}
	if v := os.Getenv("MEMORY_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MEMORY_MAX_TOKENS: want a positive integer, got %q", v)
		}
		cfg.MemoryMaxTokens = n
	}
	if v := os.Getenv("CHAT_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("CHAT_MAX_TOKENS: want a positive integer, got %q", v)
		}
		cfg.ChatMaxTokens = n
	}
	// The vector column is `vector(768)` in migration 000003. Changing the
	// model without a migration that changes the column -- and re-embeds every
	// existing chunk, since vectors from two models are not comparable -- would
	// fail on the first insert, so the knob exists but the mismatch is the
	// operator's to resolve.
	if v := os.Getenv("EMBEDDING_DIMENSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("EMBEDDING_DIMENSIONS: want a positive integer, got %q", v)
		}
		cfg.EmbeddingDimensions = n
	}
	if v := os.Getenv("MAX_UPLOAD_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("MAX_UPLOAD_BYTES: want a positive integer, got %q", v)
		}
		cfg.MaxUploadBytes = n
	}
	// The extractions and the routing call run inside the turn's budget, so
	// they cannot be allowed to eat all of it. Refusing to boot rather than
	// degrading is the same choice JWT_SECRET makes: the degraded version of
	// this is a turn that answers correctly and reports itself as failed.
	routing := time.Duration(0)
	if cfg.AgentTools {
		routing = cfg.AgentTimeout
	}
	if tail := cfg.MemoryExtractTimeout + cfg.GraphExtractTimeout + routing; tail > time.Duration(float64(cfg.ChatTimeout)*MaxExtractionShare) {
		return Config{}, fmt.Errorf(
			"CHAT_TIMEOUT (%s) leaves only %s for retrieval and generation after "+
				"MEMORY_EXTRACT_TIMEOUT (%s), GRAPH_EXTRACT_TIMEOUT (%s) and AGENT_TIMEOUT (%s), "+
				"which run inside it; raise CHAT_TIMEOUT to at least %s, or lower the others",
			cfg.ChatTimeout, cfg.ChatTimeout-tail,
			cfg.MemoryExtractTimeout, cfg.GraphExtractTimeout, routing, 2*tail)
	}

	if v := os.Getenv("LOGIN_RATE_LIMIT_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("LOGIN_RATE_LIMIT_BURST: want a positive integer, got %q", v)
		}
		cfg.LoginRateLimitBurst = n
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// floatOr reads a bounded float. The bounds are checked here rather than
// clamped silently: a temperature of 9 is a typo, and answering it with 2
// would hide the mistake until someone read the generations.
func floatOr(key string, fallback, min, max float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < min || f > max {
		return 0, fmt.Errorf("%s: want a number between %g and %g, got %q", key, min, max, v)
	}
	return f, nil
}

// boolOr reads a flag. An unparseable value is an error rather than a silent
// false: "MEMORY_EXTRACTION=no" quietly disabling the feature is exactly the
// kind of typo an operator would not find for a week.
func boolOr(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: want true or false, got %q", key, v)
	}
	return b, nil
}

func durationOr(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
