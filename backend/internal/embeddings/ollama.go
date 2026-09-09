package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is where `ollama serve` listens.
const DefaultBaseURL = "http://localhost:11434"

// DefaultModel is nomic-embed-text: 768 dimensions, 8k context, ~275MB, and
// the model the Phase 3 brief specifies. Its context window is far larger than
// a ~500-token chunk, so nothing the chunker produces gets truncated.
const DefaultModel = "nomic-embed-text"

// DefaultDimensions is nomic-embed-text's output width and the width of the
// `vector(768)` column in migration 000003.
const DefaultDimensions = 768

// ErrUnavailable reports that the model server could not be reached or refused
// the request. It is separated from a malformed response because it is the
// operator's problem, not the caller's, and the document pipeline records a
// different message for it.
var ErrUnavailable = errors.New("embedding service unavailable")

// maxBatch caps how many texts go in one request. Ollama holds the whole batch
// in memory while it runs, so an unbounded batch turns a large document into a
// memory spike on the model server.
const maxBatch = 32

// Ollama is an Embedder backed by a local Ollama server.
type Ollama struct {
	baseURL string
	model   string
	dims    int
	client  *http.Client
}

// NewOllama builds a client. An empty baseURL or model falls back to the
// defaults above.
func NewOllama(baseURL, model string, dims int, timeout time.Duration) *Ollama {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if model == "" {
		model = DefaultModel
	}
	if dims <= 0 {
		dims = DefaultDimensions
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		dims:    dims,
		// The per-request ceiling. The caller's context still governs the
		// whole pipeline; this only stops one hung request holding it open.
		client: &http.Client{Timeout: timeout},
	}
}

func (o *Ollama) Dimensions() int { return o.dims }
func (o *Ollama) Model() string   { return o.model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed returns one vector per input text, in the same order.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxBatch {
		end := min(start+maxBatch, len(texts))
		batch, err := o.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (o *Ollama) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: o.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("encode embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		// A cancelled or expired caller context is the caller's own doing;
		// reporting it as an outage would mislabel a timed-out upload.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	// Bounded read: a wrong URL pointing at something that streams forever
	// must not become unbounded memory here.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s: %s", ErrUnavailable, resp.Status, snippet(raw))
	}

	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if parsed.Error != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, parsed.Error)
	}
	if len(parsed.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: asked for %d vectors, got %d", len(texts), len(parsed.Embeddings))
	}
	// A width mismatch means the configured model is not the one the vector
	// column was sized for. Catching it here names the cause; letting it reach
	// Postgres would surface as an opaque "expected 768 dimensions" error.
	for i, v := range parsed.Embeddings {
		if len(v) != o.dims {
			return nil, fmt.Errorf("embed: model %q returned %d dimensions for input %d, want %d", o.model, len(v), i, o.dims)
		}
	}
	return parsed.Embeddings, nil
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
