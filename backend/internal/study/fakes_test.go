package study

import (
	"context"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/optional"
)

// fakeStore keys by (owner, id), the same shape the SQL has -- including the
// two ownership probes, which are the only place this module reads another
// module's rows, and the card count, which is derived rather than stored for
// the same reason the repository derives it.
type fakeStore struct {
	mu        sync.Mutex
	plans     map[uuid.UUID]map[uuid.UUID]Plan
	cards     map[uuid.UUID]map[uuid.UUID]Flashcard
	documents map[uuid.UUID]map[uuid.UUID]string
	calls     []uuid.UUID
	filters   []Filter
	batches   [][]CreateCardInput
	err       error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		plans:     map[uuid.UUID]map[uuid.UUID]Plan{},
		cards:     map[uuid.UUID]map[uuid.UUID]Flashcard{},
		documents: map[uuid.UUID]map[uuid.UUID]string{},
	}
}

func (f *fakeStore) seedDocument(owner uuid.UUID, filename string) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	if f.documents[owner] == nil {
		f.documents[owner] = map[uuid.UUID]string{}
	}
	f.documents[owner][id] = filename
	return id
}

// cardCount is the count the repository computes in SQL.
func (f *fakeStore) cardCount(owner, planID uuid.UUID) int {
	n := 0
	for _, c := range f.cards[owner] {
		if c.StudyPlanID != nil && *c.StudyPlanID == planID {
			n++
		}
	}
	return n
}

func (f *fakeStore) hydrate(owner uuid.UUID, p Plan) Plan {
	p.CardCount = f.cardCount(owner, p.ID)
	p.DocumentName = nil
	if p.DocumentID != nil {
		if name, ok := f.documents[owner][*p.DocumentID]; ok {
			p.DocumentName = &name
		}
	}
	return p
}

func (f *fakeStore) CreatePlan(_ context.Context, userID uuid.UUID, in CreatePlanInput) (Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return Plan{}, f.err
	}
	p := Plan{
		ID: uuid.New(), UserID: userID, Title: in.Title,
		DocumentID: in.DocumentID, Status: in.Status,
	}
	if in.Description != "" {
		d := in.Description
		p.Description = &d
	}
	if f.plans[userID] == nil {
		f.plans[userID] = map[uuid.UUID]Plan{}
	}
	f.plans[userID][p.ID] = p
	return f.hydrate(userID, p), nil
}

func (f *fakeStore) PlanByID(_ context.Context, userID, id uuid.UUID) (Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	p, ok := f.plans[userID][id]
	if !ok {
		return Plan{}, ErrNotFound
	}
	return f.hydrate(userID, p), nil
}

func (f *fakeStore) Plans(_ context.Context, userID uuid.UUID, filter Filter) ([]Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.filters = append(f.filters, filter)
	out := []Plan{}
	for _, p := range f.plans[userID] {
		if filter.Status != "" && p.Status != filter.Status {
			continue
		}
		if filter.DocumentID != nil && (p.DocumentID == nil || *p.DocumentID != *filter.DocumentID) {
			continue
		}
		if filter.Query != "" {
			body := p.Title
			if p.Description != nil {
				body += " " + *p.Description
			}
			if !strings.Contains(strings.ToLower(body), strings.ToLower(filter.Query)) {
				continue
			}
		}
		out = append(out, f.hydrate(userID, p))
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeStore) UpdatePlan(_ context.Context, userID, id uuid.UUID, patch Patch) (Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	p, ok := f.plans[userID][id]
	if !ok {
		return Plan{}, ErrNotFound
	}
	if patch.Title != nil {
		p.Title = *patch.Title
	}
	if patch.Status != nil {
		p.Status = *patch.Status
	}
	if patch.Description.Set {
		p.Description = patch.Description.Value
	}
	if patch.DocumentID.Set {
		p.DocumentID = patch.DocumentID.Value
	}
	f.plans[userID][id] = p
	return f.hydrate(userID, p), nil
}

func (f *fakeStore) DeletePlan(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.plans[userID][id]; !ok {
		return ErrNotFound
	}
	delete(f.plans[userID], id)
	// The ON DELETE CASCADE from study_plans.
	for cardID, c := range f.cards[userID] {
		if c.StudyPlanID != nil && *c.StudyPlanID == id {
			delete(f.cards[userID], cardID)
		}
	}
	return nil
}

func (f *fakeStore) PlanExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	_, ok := f.plans[userID][id]
	return ok, nil
}

func (f *fakeStore) DocumentExists(_ context.Context, userID, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	_, ok := f.documents[userID][id]
	return ok, nil
}

func (f *fakeStore) CreateFlashcards(_ context.Context, userID uuid.UUID, in []CreateCardInput) ([]Flashcard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.batches = append(f.batches, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.cards[userID] == nil {
		f.cards[userID] = map[uuid.UUID]Flashcard{}
	}
	out := make([]Flashcard, 0, len(in))
	for _, card := range in {
		c := Flashcard{
			ID: uuid.New(), UserID: userID, StudyPlanID: card.StudyPlanID,
			DocumentID: card.DocumentID, Front: card.Front, Back: card.Back,
		}
		f.cards[userID][c.ID] = c
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) Flashcards(_ context.Context, userID uuid.UUID, filter CardFilter) ([]Flashcard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	out := []Flashcard{}
	for _, c := range f.cards[userID] {
		if filter.StudyPlanID != nil && (c.StudyPlanID == nil || *c.StudyPlanID != *filter.StudyPlanID) {
			continue
		}
		out = append(out, c)
	}
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *fakeStore) DeleteFlashcard(_ context.Context, userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if _, ok := f.cards[userID][id]; !ok {
		return ErrFlashcardNotFound
	}
	delete(f.cards[userID], id)
	return nil
}

// otherUsers reports every owner id the store was called with that is not the
// given one. Empty is the property every test below relies on.
func (f *fakeStore) otherUsers(owner uuid.UUID) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, id := range f.calls {
		if id != owner {
			out = append(out, id)
		}
	}
	return out
}

var _ Store = (*fakeStore)(nil)

// fakeLibrary is internal/documents, in memory: a document with some passages,
// and a search over them that matches on a substring rather than on a vector.
//
// Matching on a substring is not a weaker version of the real search, it is a
// different thing -- but what these tests measure is which passages reach the
// prompt and the grounding check, not whether pgvector ranks well, which is
// internal/db's business.
type fakeLibrary struct {
	mu       sync.Mutex
	docs     map[uuid.UUID]map[uuid.UUID]documents.Document
	passages map[uuid.UUID][]string
	calls    []uuid.UUID
	queries  []documents.SearchQuery
	err      error
}

func newFakeLibrary() *fakeLibrary {
	return &fakeLibrary{
		docs:     map[uuid.UUID]map[uuid.UUID]documents.Document{},
		passages: map[uuid.UUID][]string{},
	}
}

func (f *fakeLibrary) seed(owner uuid.UUID, id uuid.UUID, filename string, passages ...string) documents.Document {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := documents.Document{
		ID: id, UserID: owner, Filename: filename,
		Status: documents.StatusReady, ChunkCount: len(passages),
	}
	if f.docs[owner] == nil {
		f.docs[owner] = map[uuid.UUID]documents.Document{}
	}
	f.docs[owner][id] = d
	f.passages[id] = passages
	return d
}

func (f *fakeLibrary) setStatus(owner, id uuid.UUID, status string, chunks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.docs[owner][id]
	d.Status, d.ChunkCount = status, chunks
	f.docs[owner][id] = d
}

func (f *fakeLibrary) Get(_ context.Context, userID, id uuid.UUID) (documents.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return documents.Document{}, f.err
	}
	d, ok := f.docs[userID][id]
	if !ok {
		return documents.Document{}, documents.ErrNotFound
	}
	return d, nil
}

func (f *fakeLibrary) List(_ context.Context, userID uuid.UUID, _ documents.Filter) ([]documents.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return nil, f.err
	}
	out := []documents.Document{}
	for _, d := range f.docs[userID] {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeLibrary) Passages(_ context.Context, userID, documentID uuid.UUID, limit int) ([]documents.Passage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.docs[userID][documentID]
	if !ok {
		return []documents.Passage{}, nil
	}
	out := []documents.Passage{}
	for i, text := range f.passages[documentID] {
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, documents.Passage{
			ChunkID: uuid.New(), DocumentID: d.ID, Filename: d.Filename,
			ChunkIndex: i, Content: text,
		})
	}
	return out, nil
}

func (f *fakeLibrary) Search(_ context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, userID)
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	out := []documents.SearchResult{}
	for _, id := range q.DocumentIDs {
		d, ok := f.docs[userID][id]
		if !ok {
			continue
		}
		for i, text := range f.passages[id] {
			if !strings.Contains(strings.ToLower(text), strings.ToLower(q.Query)) {
				continue
			}
			out = append(out, documents.SearchResult{
				ChunkID: uuid.New(), DocumentID: d.ID, Filename: d.Filename,
				ChunkIndex: i, Content: text, Similarity: 1,
			})
		}
	}
	// Reversed, so a test can see that the service puts them back in document
	// order before they reach the prompt.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

var _ Library = (*fakeLibrary)(nil)

// fakeSyncer records every call the write path makes into the knowledge graph,
// and collapses them the way the upsert does, so a test can see "one node per
// row" rather than only "SyncNode was called".
type fakeSyncer struct {
	mu    sync.Mutex
	calls []syncCall
}

type syncCall struct {
	userID   uuid.UUID
	refTable string
	refID    uuid.UUID
	label    string
}

func (f *fakeSyncer) SyncNode(_ context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, syncCall{userID: userID, refTable: refTable, refID: refID, label: label})
}

func (f *fakeSyncer) seen() []syncCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncCall(nil), f.calls...)
}

func (f *fakeSyncer) nodes() map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, c := range f.seen() {
		out[c.refID] = c.label
	}
	return out
}

var _ NodeSyncer = (*fakeSyncer)(nil)

// The document the generation tests run against. It is deliberately short and
// factual: every assertion about grounding below is an assertion about these
// two sentences.
const (
	auroraPassage = "The aurora borealis appeared over the tundra shortly after midnight " +
		"and lasted about forty minutes. The dogs slept through the whole thing."
	generatorPassage = "The generator needs a new fuel filter before the next resupply run, " +
		"and the shortwave antenna guy-line on the north side has gone slack again."
)

// planWithDescription is the input the tests reuse for an ordinary plan.
func planWithDescription() CreatePlanInput {
	return CreatePlanInput{Title: "Linear algebra finals", Description: "Eigenvalues and SVD"}
}

// clearedDescription is a PATCH that removes the description.
func clearedDescription() UpdatePlanInput {
	return UpdatePlanInput{Description: optional.Null[string]()}
}
