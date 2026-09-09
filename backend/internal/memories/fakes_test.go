package memories

import (
	"context"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The fakes key everything by (owner, id), the same shape the SQL has, so a
// service that forgot to pass the caller's id shows up here as a miss rather
// than as data belonging to nobody.

type fakeStore struct {
	mu      sync.Mutex
	byUser  map[uuid.UUID][]Memory
	vectors map[uuid.UUID][]float32
	// callers records the user id every method was given.
	callers []uuid.UUID
	// createErr fails the write, after extraction and embedding succeeded.
	createErr error
	searchErr error
	// created records what was handed to Create, embeddings included.
	created []CreateInput
	clock   time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byUser:  map[uuid.UUID][]Memory{},
		vectors: map[uuid.UUID][]float32{},
		clock:   time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeStore) Create(_ context.Context, userID uuid.UUID, in CreateInput) (Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.createErr != nil {
		return Memory{}, f.createErr
	}
	f.created = append(f.created, in)
	f.clock = f.clock.Add(time.Second)
	m := Memory{
		ID: uuid.New(), UserID: userID, Type: in.Type, Content: in.Content,
		Importance: in.Importance, Confidence: in.Confidence,
		SourceConversationID: in.SourceConversationID, Enabled: true,
		CreatedAt: f.clock, UpdatedAt: f.clock,
	}
	f.byUser[userID] = append(f.byUser[userID], m)
	f.vectors[m.ID] = in.Embedding
	return m, nil
}

// seed writes a memory directly, the way an earlier conversation would have.
func (f *fakeStore) seed(userID uuid.UUID, kind, content string) Memory {
	m, _ := f.Create(context.Background(), userID, CreateInput{
		Candidate: Candidate{Type: kind, Content: content, Importance: 0.7, Confidence: 0.9},
		Embedding: fakeVector(content),
	})
	return m
}

func (f *fakeStore) ByID(_ context.Context, userID, id uuid.UUID) (Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for _, m := range f.byUser[userID] {
		if m.ID == id {
			return m, nil
		}
	}
	return Memory{}, ErrNotFound
}

func (f *fakeStore) List(_ context.Context, userID uuid.UUID, filter Filter) ([]Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	out := []Memory{}
	for _, m := range f.byUser[userID] {
		if filter.Type != "" && m.Type != filter.Type {
			continue
		}
		if filter.Enabled != nil && m.Enabled != *filter.Enabled {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeStore) Update(_ context.Context, userID, id uuid.UUID, p Patch, embedding []float32) (Memory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for i, m := range f.byUser[userID] {
		if m.ID != id {
			continue
		}
		if p.Content != nil {
			m.Content = *p.Content
		}
		if p.Enabled != nil {
			m.Enabled = *p.Enabled
		}
		if embedding != nil {
			f.vectors[id] = embedding
		}
		f.clock = f.clock.Add(time.Second)
		m.UpdatedAt = f.clock
		f.byUser[userID][i] = m
		return m, nil
	}
	return Memory{}, ErrNotFound
}

func (f *fakeStore) Delete(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	for i, m := range f.byUser[userID] {
		if m.ID == id {
			f.byUser[userID] = append(f.byUser[userID][:i], f.byUser[userID][i+1:]...)
			delete(f.vectors, id)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) DeleteAll(_ context.Context, userID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	n := len(f.byUser[userID])
	for _, m := range f.byUser[userID] {
		delete(f.vectors, m.ID)
	}
	delete(f.byUser, userID)
	return n, nil
}

// Search is real cosine similarity over the seeded vectors, so the duplicate
// check and the retrieval floor are exercised rather than stubbed.
func (f *fakeStore) Search(_ context.Context, userID uuid.UUID, embedding []float32, q SearchQuery) ([]SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, userID)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	out := []SearchResult{}
	for _, m := range f.byUser[userID] {
		if !m.Enabled {
			continue
		}
		sim := cosine(embedding, f.vectors[m.ID])
		if sim < q.MinSimilarity {
			continue
		}
		out = append(out, SearchResult{Memory: m, Similarity: sim})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeStore) all(userID uuid.UUID) []Memory {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Memory(nil), f.byUser[userID]...)
}

func (f *fakeStore) writes() []CreateInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CreateInput(nil), f.created...)
}

func (f *fakeStore) callersSeen() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.callers...)
}

// --- embedding --------------------------------------------------------------

// fakeEmbedder is the same bag-of-words stand-in the API tests use, at a width
// small enough to read: two texts that share words are close, two that share
// none are orthogonal. That is enough to exercise the duplicate check and the
// similarity floor without a model server.
type fakeEmbedder struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

const fakeDimensions = 64

func (e *fakeEmbedder) Dimensions() int { return fakeDimensions }
func (e *fakeEmbedder) Model() string   { return "fake-embedder" }

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls = append(e.calls, append([]string(nil), texts...))
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = fakeVector(t)
	}
	return out, nil
}

func (e *fakeEmbedder) embedded() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.calls...)
}

func fakeVector(text string) []float32 {
	v := make([]float32, fakeDimensions)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(word, ".,;:!?\"'()[]")))
		v[h.Sum32()%uint32(len(v))]++
	}
	var norm float64
	for _, f := range v {
		norm += float64(f) * float64(f)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range v {
			v[i] = float32(float64(v[i]) / norm)
		}
	}
	return v
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
