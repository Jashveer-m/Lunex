package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOllama stands in for `ollama serve`.
type fakeOllama struct {
	mu      sync.Mutex
	batches [][]string
	dims    int
	status  int
	body    string
}

func (f *fakeOllama) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.batches = append(f.batches, req.Input)
		f.mu.Unlock()

		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}

		out := make([][]float32, len(req.Input))
		for i := range out {
			out[i] = make([]float32, f.dims)
			out[i][0] = float32(i + 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(embedResponse{Embeddings: out})
	}
}

func newFakeOllama(t *testing.T, dims int) (*Ollama, *fakeOllama) {
	t.Helper()
	fake := &fakeOllama{dims: dims}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return NewOllama(srv.URL, "test-model", DefaultDimensions, 5*time.Second), fake
}

func TestEmbedReturnsOneVectorPerInput(t *testing.T) {
	client, fake := newFakeOllama(t, DefaultDimensions)

	got, err := client.Embed(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d vectors, want 3", len(got))
	}
	for i, v := range got {
		if len(v) != DefaultDimensions {
			t.Fatalf("vector %d has width %d, want %d", i, len(v), DefaultDimensions)
		}
	}
	if len(fake.batches) != 1 {
		t.Fatalf("made %d requests, want the three inputs batched into 1", len(fake.batches))
	}
}

func TestEmbedEmptyInputMakesNoRequest(t *testing.T) {
	client, fake := newFakeOllama(t, DefaultDimensions)
	got, err := client.Embed(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("Embed(nil) = %v, %v; want nil, nil", got, err)
	}
	if len(fake.batches) != 0 {
		t.Fatal("an empty batch must not reach the model server")
	}
}

// A long document must not become one enormous request the model server holds
// entirely in memory -- and the order of the results must survive the split.
func TestEmbedSplitsLargeBatchesInOrder(t *testing.T) {
	client, fake := newFakeOllama(t, DefaultDimensions)

	texts := make([]string, maxBatch*2+5)
	for i := range texts {
		texts[i] = strings.Repeat("x", i+1)
	}
	got, err := client.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d vectors for %d inputs", len(got), len(texts))
	}
	if len(fake.batches) != 3 {
		t.Fatalf("made %d requests, want 3 batches of at most %d", len(fake.batches), maxBatch)
	}

	var seen []string
	for _, b := range fake.batches {
		if len(b) > maxBatch {
			t.Fatalf("a batch carried %d inputs, over the %d cap", len(b), maxBatch)
		}
		seen = append(seen, b...)
	}
	for i := range texts {
		if seen[i] != texts[i] {
			t.Fatalf("input %d arrived as %q, want %q -- order decides which chunk each vector belongs to", i, seen[i], texts[i])
		}
	}
}

// A width mismatch means the configured model is not the one the vector column
// was sized for. Catching it here names the cause; Postgres would not.
func TestEmbedRejectsAWrongWidth(t *testing.T) {
	client, _ := newFakeOllama(t, 384)

	_, err := client.Embed(context.Background(), []string{"one"})
	if err == nil {
		t.Fatal("want an error when the model returns the wrong number of dimensions")
	}
	if !strings.Contains(err.Error(), "384") || !strings.Contains(err.Error(), "768") {
		t.Fatalf("error = %v, want it to name both widths", err)
	}
}

func TestEmbedReportsAnOutage(t *testing.T) {
	client, fake := newFakeOllama(t, DefaultDimensions)
	fake.status, fake.body = http.StatusInternalServerError, `{"error":"model not found"}`

	_, err := client.Embed(context.Background(), []string{"one"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestEmbedReportsAnUnreachableServer(t *testing.T) {
	// A port nothing is listening on.
	client := NewOllama("http://127.0.0.1:1", "test-model", DefaultDimensions, time.Second)
	_, err := client.Embed(context.Background(), []string{"one"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

// A cancelled caller is the caller's own doing; reporting it as an outage
// would have the document say the model server is down when it is not.
func TestEmbedDistinguishesCancellationFromAnOutage(t *testing.T) {
	client, _ := newFakeOllama(t, DefaultDimensions)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Embed(ctx, []string{"one"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want it not to be reported as an outage", err)
	}
}

func TestNewOllamaDefaults(t *testing.T) {
	c := NewOllama("", "", 0, 0)
	if c.baseURL != DefaultBaseURL || c.Model() != DefaultModel || c.Dimensions() != DefaultDimensions {
		t.Fatalf("defaults = %s %s %d", c.baseURL, c.Model(), c.Dimensions())
	}
	// A trailing slash on the configured URL must not produce `//api/embed`.
	if got := NewOllama("http://host:1234/", "m", 1, time.Second); got.baseURL != "http://host:1234" {
		t.Fatalf("baseURL = %q, want the trailing slash trimmed", got.baseURL)
	}
}
