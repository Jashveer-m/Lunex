package ai

import (
	"context"
	"strings"
	"sync"
)

// Mock is a deterministic Provider.
//
// It lives in the package rather than in a _test.go file on purpose: the chat
// orchestrator, the router and the cross-user isolation tests all need a model
// that answers without Ollama running, and a test helper that three packages
// import is production code with a small audience. It is also the reason
// `go test ./...` is green on a machine with no model server.
//
// Beyond canned answers it records every prompt it was given, which is what
// makes the orchestrator's behaviour assertable: whether the system prompt
// carried the grounding rules, whether the retrieved context reached the
// model, and whose data was in it.
type Mock struct {
	// Reply is what every call answers with. Empty means DefaultMockReply.
	Reply string
	// ReplyFunc, when set, wins over Reply and derives the answer from the
	// prompt -- which is how a test makes the model "cite" a source it was
	// actually given.
	ReplyFunc func(msgs []Message) string
	// Err fails the Chat call itself, the way an unreachable server does.
	Err error
	// StreamErr fails the stream after its first piece, the way a generation
	// cut off partway does.
	StreamErr error
	// ModelName is reported by Model(). Empty means "mock".
	ModelName string

	mu    sync.Mutex
	calls []Call
}

// DefaultMockReply is the answer with no Reply or ReplyFunc set.
const DefaultMockReply = "This is a canned reply from the mock provider."

// Call is one recorded request.
type Call struct {
	Messages []Message
	Options  Options
}

var _ Provider = (*Mock)(nil)

func (m *Mock) Model() string {
	if m.ModelName != "" {
		return m.ModelName
	}
	return "mock"
}

func (m *Mock) Chat(ctx context.Context, msgs []Message, opts Options) (Stream, error) {
	m.mu.Lock()
	m.calls = append(m.calls, Call{Messages: append([]Message(nil), msgs...), Options: opts})
	m.mu.Unlock()

	if m.Err != nil {
		return nil, m.Err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	reply := m.Reply
	if m.ReplyFunc != nil {
		reply = m.ReplyFunc(msgs)
	}
	if reply == "" && m.ReplyFunc == nil {
		reply = DefaultMockReply
	}
	// Split on whitespace but keep it, so reassembling the pieces gives back
	// exactly the reply -- the property a streaming client depends on.
	return &mockStream{ctx: ctx, pieces: splitKeepingSpace(reply), err: m.StreamErr}, nil
}

// Calls returns every prompt the mock has been given, in order.
func (m *Mock) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

// LastPrompt returns the most recent prompt, or nil if there was none.
func (m *Mock) LastPrompt() []Message {
	calls := m.Calls()
	if len(calls) == 0 {
		return nil
	}
	return calls[len(calls)-1].Messages
}

// PromptText joins a prompt into one string, for the common assertion of
// "did this text reach the model at all".
func PromptText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

type mockStream struct {
	ctx    context.Context
	pieces []string
	i      int
	cur    string
	err    error
	fired  bool
}

func (s *mockStream) Next() bool {
	if err := s.ctx.Err(); err != nil {
		s.err, s.cur = err, ""
		return false
	}
	// A configured StreamErr fires after one piece has been delivered, so a
	// test can distinguish "the call failed" from "the answer was truncated".
	if s.err != nil && s.i > 0 && !s.fired {
		s.fired, s.cur = true, ""
		return false
	}
	if s.i >= len(s.pieces) {
		if s.err != nil && !s.fired {
			s.fired = true
		}
		s.cur = ""
		return false
	}
	s.cur = s.pieces[s.i]
	s.i++
	return true
}

func (s *mockStream) Text() string {
	return s.cur
}

func (s *mockStream) Err() error {
	if s.fired {
		return s.err
	}
	return nil
}

func (s *mockStream) Close() error { return nil }

// splitKeepingSpace cuts a string into word-sized pieces whose concatenation
// is the original, so streaming it loses nothing.
func splitKeepingSpace(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i] == ' ' && s[i-1] != ' ' {
			out = append(out, s[start:i])
			start = i
		}
	}
	return append(out, s[start:])
}
