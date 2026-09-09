package api_test

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"sync"

	"github.com/jashveer/lifeos/backend/internal/embeddings"
)

// hashingEmbedder is a deterministic stand-in for Ollama.
//
// It hashes each word into one of 768 buckets and returns the normalized
// counts, which makes it a real bag-of-words embedding: two texts that share
// words are close, two that share none are orthogonal. That is enough for the
// tests here, which ask whether the right chunk comes back and whose chunks
// were searched -- not whether the model understands the sentence.
//
// Using it keeps the isolation and routing tests free of a running model
// server. The genuine end-to-end check against Ollama is in docs/testing.md.
type hashingEmbedder struct {
	mu    sync.Mutex
	calls int
}

var _ embeddings.Embedder = (*hashingEmbedder)(nil)

func (e *hashingEmbedder) Dimensions() int { return embeddings.DefaultDimensions }
func (e *hashingEmbedder) Model() string   { return "hashing-test-embedder" }

func (e *hashingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, embeddings.DefaultDimensions)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(strings.Trim(word, ".,;:!?\"'()[]")))
			v[h.Sum32()%uint32(len(v))]++
		}
		var norm float64
		for _, f := range v {
			norm += float64(f) * float64(f)
		}
		// An all-whitespace text would divide by zero; the chunker never
		// produces one, but a zero vector is the honest answer if it did.
		if norm > 0 {
			norm = math.Sqrt(norm)
			for j := range v {
				v[j] = float32(float64(v[j]) / norm)
			}
		}
		out[i] = v
	}
	return out, nil
}
