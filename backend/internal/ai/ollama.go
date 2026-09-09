package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is where `ollama serve` listens. It is spelled out here
// rather than imported from internal/embeddings: the two happen to point at
// the same process today, but "where the chat model lives" and "where the
// embedding model lives" are separate knobs, and config wires both from
// OLLAMA_BASE_URL only because that is the local setup.
const DefaultBaseURL = "http://localhost:11434"

// DefaultModel is the model the Phase 4 brief specifies: small enough to
// answer on a laptop CPU, large enough to follow the grounding rules in the
// system prompt. It is a default, not a constant of the design -- CHAT_MODEL
// overrides it.
const DefaultModel = "llama3.2:3b"

// maxStreamBytes bounds one reply. A wrong base URL pointing at something that
// streams forever must not become unbounded memory, and MaxTokens is the
// model's own limit rather than a guarantee about bytes on the wire.
const maxStreamBytes = 8 << 20

// Ollama is a Provider backed by a local Ollama server's /api/chat.
type Ollama struct {
	baseURL string
	model   string
	client  *http.Client
}

var _ Provider = (*Ollama)(nil)

// NewOllama builds a client. An empty baseURL or model falls back to the
// defaults above; a non-positive timeout to five minutes.
//
// Nothing is dialled here: the model server being down is a per-request
// failure the chat endpoint reports as a 503, not a reason for the process to
// refuse to boot.
func NewOllama(baseURL, model string, timeout time.Duration) *Ollama {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		// The per-request ceiling, covering the whole stream rather than just
		// the headers. The caller's context still governs the turn; this only
		// stops one hung generation holding a connection open forever.
		client: &http.Client{Timeout: timeout},
	}
}

func (o *Ollama) Model() string { return o.model }

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  *wireOptions  `json:"options,omitempty"`
}

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
}

// chatFrame is one newline-delimited JSON object from /api/chat. Ollama sends
// one per token-ish fragment, then a final frame with done=true.
type chatFrame struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
}

// Chat opens a streaming completion. The error it returns covers everything
// that fails before generation starts: an unreachable server, an unknown
// model, a refused request.
func (o *Ollama) Chat(ctx context.Context, msgs []Message, opts Options) (Stream, error) {
	if len(msgs) == 0 {
		return nil, fmt.Errorf("chat: no messages")
	}

	wire := make([]wireMessage, len(msgs))
	for i, m := range msgs {
		wire[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	req := chatRequest{Model: o.model, Messages: wire, Stream: true}
	if opts.Model != "" {
		req.Model = opts.Model
	}
	// Only knobs the caller actually set are sent, so the model's own defaults
	// stay in force for the rest rather than being overwritten with zeroes.
	if opts.Temperature > 0 || opts.MaxTokens > 0 {
		req.Options = &wireOptions{}
		if opts.Temperature > 0 {
			t := opts.Temperature
			req.Options.Temperature = &t
		}
		if opts.MaxTokens > 0 {
			n := opts.MaxTokens
			req.Options.NumPredict = &n
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build chat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(httpReq)
	if err != nil {
		// A cancelled or expired caller context is the caller's own doing;
		// reporting it as an outage would mislabel an abandoned request.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("%w: %s: %s", ErrUnavailable, resp.Status, snippet(raw))
	}

	return &ollamaStream{
		ctx:  ctx,
		resp: resp,
		dec:  json.NewDecoder(io.LimitReader(resp.Body, maxStreamBytes)),
	}, nil
}

type ollamaStream struct {
	ctx     context.Context
	resp    *http.Response
	dec     *json.Decoder
	cur     string
	err     error
	done    bool
	sawDone bool
}

func (s *ollamaStream) Text() string { return s.cur }
func (s *ollamaStream) Err() error   { return s.err }

func (s *ollamaStream) Close() error {
	if s.resp == nil {
		return nil
	}
	// Drain what is left so the connection can be reused rather than reset.
	// The bound is small: this only runs on a stream the caller abandoned.
	_, _ = io.Copy(io.Discard, io.LimitReader(s.resp.Body, 1<<20))
	return s.resp.Body.Close()
}

// Next decodes frames until one carries text, the model says it is done, or
// something goes wrong.
func (s *ollamaStream) Next() bool {
	if s.done {
		return false
	}
	for {
		var frame chatFrame
		if err := s.dec.Decode(&frame); err != nil {
			s.done, s.cur = true, ""
			switch {
			case s.sawDone:
				// The model finished and the body ended. Nothing to report.
			case s.ctx.Err() != nil:
				s.err = s.ctx.Err()
			default:
				// A body that ends without done=true is a truncated answer,
				// not a complete one. Saying so is what keeps a half-generated
				// reply from being persisted as if the model had finished.
				s.err = fmt.Errorf("%w: the model stream ended before it finished: %v", ErrUnavailable, err)
			}
			return false
		}
		// Ollama reports a mid-stream problem in-band rather than by status.
		if frame.Error != "" {
			s.done, s.cur = true, ""
			s.err = fmt.Errorf("%w: %s", ErrUnavailable, frame.Error)
			return false
		}
		if frame.Done {
			s.sawDone = true
		}
		if frame.Message.Content != "" {
			s.cur = frame.Message.Content
			s.done = frame.Done
			return true
		}
		if frame.Done {
			s.done, s.cur = true, ""
			return false
		}
		// An empty, not-done frame: keep reading.
	}
}

// snippet keeps an upstream error body short enough to log.
func snippet(b []byte) string {
	const limit = 200
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
