// Package embeddings turns text into vectors.
//
// The only implementation is Ollama, but the package exposes the operation as
// a small interface so the document pipeline can be tested without a model
// server running, and so a hosted provider can be swapped in later without the
// documents package learning about it.
package embeddings

import "context"

// Embedder converts a batch of texts into vectors, one per input, in order.
//
// Batching is part of the interface rather than a convenience wrapper: a
// document is chunked into tens of pieces at a time, and a per-chunk round
// trip would spend most of the upload in HTTP overhead.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions is the vector width this embedder produces. It must match the
	// `vector(n)` column, so it is checked at boot rather than discovered on
	// the first insert.
	Dimensions() int
	// Model names the model, for logging and for the health of a later
	// re-embedding migration: vectors from two models are not comparable.
	Model() string
}
