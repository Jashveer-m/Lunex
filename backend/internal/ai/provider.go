// Package ai is the abstraction over the chat model.
//
// It exists for the same reason internal/embeddings does: the orchestrator in
// internal/chat should depend on "something that turns a prompt into tokens",
// not on Ollama's wire format. Two implementations ship in this phase --
// Ollama, and a deterministic Mock the tests use so the suite needs no model
// server -- and the interface is deliberately shaped so a hosted provider
// (Anthropic, OpenAI) can be added later without the chat package changing:
// everything provider-specific is either behind Chat or inside Options.
//
// No cloud provider is implemented here. See docs/decisions.md.
package ai

import (
	"context"
	"errors"
	"strings"
)

// The roles a prompt message can carry. They are the three every chat model
// agrees on; anything richer (tool calls, images) is a later phase.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Roles is the allow-list, used by the messages table's CHECK constraint and
// by validation.
var Roles = []string{RoleSystem, RoleUser, RoleAssistant}

// ErrUnavailable reports that the model could not be reached, refused the
// request, or cut the stream off partway.
//
// It is separated from a programming error for the same reason
// embeddings.ErrUnavailable is: it is the operator's problem, it maps to a 503
// rather than a 500, and a client should retry on it.
var ErrUnavailable = errors.New("chat model unavailable")

// Message is one turn of the prompt.
type Message struct {
	Role    string
	Content string
}

// Options are the per-request knobs. A zero Options is valid and means "the
// provider's own defaults", so a caller that has no opinion states none.
type Options struct {
	// Model overrides the provider's configured model for this call.
	Model string
	// Temperature is the sampling temperature. Zero means the provider
	// default -- not greedy decoding -- so use a small positive number to ask
	// for near-deterministic output.
	Temperature float64
	// MaxTokens caps the reply length. Zero or less means the provider's own
	// limit.
	MaxTokens int
	// Format constrains the shape of the reply. Empty means free text;
	// FormatJSON asks the provider to emit well-formed JSON and nothing else.
	//
	// It exists because one caller -- the memory extractor -- is a classifier
	// rather than a conversation: it asks the model for a list of facts and has
	// to parse the answer. Asking for JSON in the prompt gets valid JSON most
	// of the time; a provider that can constrain decoding gets it nearly
	// always, and the difference is a fact remembered or lost. A provider that
	// does not support it ignores the field, so callers still parse defensively.
	Format string
}

// FormatJSON is the only Format value in this phase.
const FormatJSON = "json"

// Provider is the model. Chat sends a whole prompt and returns the reply as a
// Stream, because a local 3B model takes seconds to produce a paragraph and a
// user should not watch a blank screen for all of them.
//
// Chat returns as soon as the model has accepted the request -- for Ollama,
// once the response headers are back. That is what lets the HTTP layer answer
// a refused request with a real status code: everything that can fail before
// the first token fails here, and only a mid-generation failure has to be
// reported inside an already-started stream.
type Provider interface {
	Chat(ctx context.Context, msgs []Message, opts Options) (Stream, error)
	// Model names the configured model, for logging and for the response
	// metadata. Answers from two models are not interchangeable, so which one
	// produced a message is worth being able to say.
	Model() string
}

// Stream is the reply arriving in pieces.
//
// The iterator shape is the one that suits both implementations: Ollama sends
// newline-delimited JSON frames that are decoded one at a time, and a caller
// that wants the whole answer can use Collect. Close is mandatory -- it
// releases the underlying HTTP connection -- so callers defer it.
type Stream interface {
	// Next advances to the next piece of text. It returns false at the end of
	// the reply and on failure; Err says which.
	Next() bool
	// Text is the piece Next just advanced to. It is a fragment, not a whole
	// line or word.
	Text() string
	// Err is nil on a stream that finished normally.
	Err() error
	Close() error
}

// Collect drains a stream into one string. It is what a caller that does not
// need incremental output uses, and what the tests use.
func Collect(s Stream) (string, error) {
	var b strings.Builder
	for s.Next() {
		b.WriteString(s.Text())
	}
	return b.String(), s.Err()
}
