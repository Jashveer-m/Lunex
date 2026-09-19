package study

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/ai"
	"github.com/jashveer/lifeos/backend/internal/documents"
	"github.com/jashveer/lifeos/backend/internal/validate"
)

// Store is the slice of the repository the service needs. As everywhere else,
// every method takes the owner id: there is no way to reach a plan or a card
// without saying whose it is, which is what makes the isolation property
// structural rather than a rule each call site has to remember.
type Store interface {
	CreatePlan(ctx context.Context, userID uuid.UUID, in CreatePlanInput) (Plan, error)
	PlanByID(ctx context.Context, userID, id uuid.UUID) (Plan, error)
	Plans(ctx context.Context, userID uuid.UUID, f Filter) ([]Plan, error)
	UpdatePlan(ctx context.Context, userID, id uuid.UUID, p Patch) (Plan, error)
	DeletePlan(ctx context.Context, userID, id uuid.UUID) error
	PlanExists(ctx context.Context, userID, id uuid.UUID) (bool, error)
	// DocumentExists reports whether the caller owns the document a plan or a
	// card points at. Owner-scoped, so another user's document is
	// indistinguishable from one that does not exist.
	DocumentExists(ctx context.Context, userID, id uuid.UUID) (bool, error)

	CreateFlashcards(ctx context.Context, userID uuid.UUID, in []CreateCardInput) ([]Flashcard, error)
	Flashcards(ctx context.Context, userID uuid.UUID, f CardFilter) ([]Flashcard, error)
	DeleteFlashcard(ctx context.Context, userID, id uuid.UUID) error
}

// Library is the slice of internal/documents this module reads.
//
// Unlike the NodeSyncer below, this one is declared over another package's
// types rather than over strings, and that is not an oversight: what study
// needs from documents is the text of a document, and there is no way to say
// "the text of a document" in this package's own vocabulary that would not be
// documents.Passage with a different name on it. The dependency is the phase --
// "document-grounded flashcards" is the feature -- so it is declared plainly
// instead of being laundered through a translation layer. See
// docs/decisions.md.
//
// Every method takes the owner id, for the same reason every Store method
// does.
type Library interface {
	Get(ctx context.Context, userID, id uuid.UUID) (documents.Document, error)
	List(ctx context.Context, userID uuid.UUID, f documents.Filter) ([]documents.Document, error)
	Passages(ctx context.Context, userID, documentID uuid.UUID, limit int) ([]documents.Passage, error)
	Search(ctx context.Context, userID uuid.UUID, q documents.SearchQuery) ([]documents.SearchResult, error)
}

// NodeSyncer mirrors a study plan into the personal knowledge graph. Phase 6's
// internal/graph implements it; nil means no graph is wired and this module
// behaves as a plain CRUD resource.
//
// The interface is declared here rather than imported, the same way every
// other resource module declares it: this package depends on "something that
// records a plan in the graph", not on the graph package.
//
// SyncNode returns no error on purpose, and there is deliberately no delete
// counterpart -- the node goes with the row through the trigger migration
// 000010 puts on this table. See notes.NodeSyncer for the argument.
type NodeSyncer interface {
	SyncNode(ctx context.Context, userID uuid.UUID, refTable string, refID uuid.UUID, label string)
}

// graphRefTable is what this module's nodes record in `ref_table`. It matches
// the allow-list in migration 000010 and graph's own map; a mismatch is a
// write error, and the round trip is pinned in internal/db's Phase 10 tests.
const graphRefTable = "study_plans"

// Options are the generation knobs, resolved from configuration at boot.
type Options struct {
	// Model overrides the provider's own model for generation. A bigger model
	// than the one answering the user is the reasonable choice here, which is
	// the opposite of the memory extractor's advice and for a plain reason:
	// the output of this call is read by a person, over and over, as a study
	// aid.
	Model string
	// Temperature is near-zero by default. Writing a card from a passage is a
	// reading task, and a creatively rephrased answer is one the grounding
	// check will throw away anyway.
	Temperature float64
	// MaxTokens caps the reply. Twenty short cards as JSON is around a
	// thousand tokens; the cap is what stops a model that decided to explain
	// the document from generating for a minute.
	MaxTokens int
	// Timeout bounds one generation. It is what the user waits while the
	// assistant prepares the proposal, so it comes out of the chat turn's own
	// budget the way the routing call does.
	Timeout time.Duration
	// JSONMode asks the provider to constrain decoding to well-formed JSON. It
	// is off by default for the reason memories.Options.JSONMode gives: on
	// llama3.2:3b the constraint makes the output measurably worse, and the
	// reply is parsed defensively either way.
	JSONMode bool
}

// Generation defaults, applied to a zero Options.
const (
	DefaultGenerationTemperature = 0.1
	DefaultGenerationMaxTokens   = 1024
	DefaultGenerationTimeout     = 180 * time.Second
)

// Deps are everything the service needs.
//
// Provider, Library and Graph are each optional in a different way. Without a
// Provider or a Library, generation answers ErrGeneration and the rest of the
// module works -- which is the service the HTTP API alone needs. Without a
// Graph, plans get no node, which is how every resource module here behaves.
type Deps struct {
	Store    Store
	Library  Library
	Provider ai.Provider
	Graph    NodeSyncer
	Logger   *slog.Logger
	Options  Options
}

// Service holds the study use cases. It is transport agnostic:
// ProposeFlashcards is called by the tool registry as a plain function, with
// no HTTP in the way.
type Service struct {
	store   Store
	library Library
	model   ai.Provider
	graph   NodeSyncer
	log     *slog.Logger
	opts    Options
}

func NewService(d Deps) *Service {
	opts := d.Options
	if opts.Temperature == 0 {
		opts.Temperature = DefaultGenerationTemperature
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = DefaultGenerationMaxTokens
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultGenerationTimeout
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: d.Store, library: d.Library, model: d.Provider,
		graph: d.Graph, log: log, opts: opts,
	}
}

// --- plans ---------------------------------------------------------------------

// sync mirrors a plan into the graph, on create and on update alike -- on
// update because the node's label is the plan's title, and a node still
// carrying the old title is one the chat mention scan matches on a name the
// user has stopped using. It also makes the update path the repair for a
// create whose sync failed.
func (s *Service) sync(ctx context.Context, userID uuid.UUID, p Plan) {
	if s.graph == nil {
		return
	}
	s.graph.SyncNode(ctx, userID, graphRefTable, p.ID, p.Title)
}

func (s *Service) CreatePlan(ctx context.Context, userID uuid.UUID, in CreatePlanInput) (Plan, error) {
	in, err := ValidateCreatePlan(in)
	if err != nil {
		return Plan{}, err
	}
	if err := s.requireOwnedDocument(ctx, userID, in.DocumentID); err != nil {
		return Plan{}, err
	}
	created, err := s.store.CreatePlan(ctx, userID, in)
	if err != nil {
		return Plan{}, linkRace(err)
	}
	s.sync(ctx, userID, created)
	return created, nil
}

func (s *Service) Plan(ctx context.Context, userID, id uuid.UUID) (Plan, error) {
	return s.store.PlanByID(ctx, userID, id)
}

func (s *Service) Plans(ctx context.Context, userID uuid.UUID, f Filter) ([]Plan, error) {
	f, err := ValidateFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.Plans(ctx, userID, f)
}

func (s *Service) UpdatePlan(ctx context.Context, userID, id uuid.UUID, in UpdatePlanInput) (Plan, error) {
	p, err := ValidatePatch(in)
	if err != nil {
		return Plan{}, err
	}
	if v, ok := p.DocumentID.Get(); ok {
		if err := s.requireOwnedDocument(ctx, userID, &v); err != nil {
			return Plan{}, err
		}
	}
	updated, err := s.store.UpdatePlan(ctx, userID, id, p)
	if err != nil {
		return Plan{}, linkRace(err)
	}
	s.sync(ctx, userID, updated)
	return updated, nil
}

// DeletePlan removes a plan and, through the cascade, its cards. It makes no
// graph call: the node goes with the row in the same statement, through the
// trigger migration 000010 puts on this table.
func (s *Service) DeletePlan(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.DeletePlan(ctx, userID, id)
}

// --- flashcards ------------------------------------------------------------------

// Flashcards lists one plan's cards.
//
// The plan is loaded first, so a plan that is not the caller's is a 404 rather
// than an empty deck: "no cards" and "no such plan" are different answers, and
// only the first is true of an empty plan somebody owns.
func (s *Service) Flashcards(ctx context.Context, userID, planID uuid.UUID, f CardFilter) ([]Flashcard, error) {
	if _, err := s.store.PlanByID(ctx, userID, planID); err != nil {
		return nil, err
	}
	f, err := ValidateCardFilter(f)
	if err != nil {
		return nil, err
	}
	f.StudyPlanID = &planID
	return s.store.Flashcards(ctx, userID, f)
}

// AddFlashcard writes one card the user typed, under one of their plans.
//
// It is the direct path, with no approval in front of it, and that is right:
// the user is the one asking. Approval stands between the *assistant* and the
// user's data, not between the user and their own.
func (s *Service) AddFlashcard(ctx context.Context, userID, planID uuid.UUID, card NewCard) (Flashcard, error) {
	if _, err := s.store.PlanByID(ctx, userID, planID); err != nil {
		return Flashcard{}, err
	}
	card, err := ValidateCard(card)
	if err != nil {
		return Flashcard{}, err
	}
	// No document_id: a card the user wrote has no source to trace to, and
	// borrowing the plan's document would claim a provenance it does not have.
	// The whole point of that column is that it means "this came from there".
	created, err := s.store.CreateFlashcards(ctx, userID, []CreateCardInput{{
		StudyPlanID: &planID, Front: card.Front, Back: card.Back,
	}})
	if err != nil {
		return Flashcard{}, linkRace(err)
	}
	if len(created) != 1 {
		return Flashcard{}, fmt.Errorf("adding one flashcard wrote %d", len(created))
	}
	return created[0], nil
}

// CreateFlashcards writes a batch of cards, checking every link first.
//
// This is what an approved generation runs. It takes cards rather than a
// document and a count: the cards were written when the proposal was made and
// are what the user approved, and generating again here would store something
// nobody had read. See the package comment.
func (s *Service) CreateFlashcards(ctx context.Context, userID uuid.UUID, in []CreateCardInput) ([]Flashcard, error) {
	if len(in) == 0 {
		return nil, validate.Errors{{Field: "flashcards", Message: "is required"}}
	}
	if len(in) > MaxCardsPerBatch {
		return nil, validate.Errors{{
			Field:   "flashcards",
			Message: fmt.Sprintf("must be at most %d at a time", MaxCardsPerBatch),
		}}
	}
	// Every card in a generated batch names the same plan and the same
	// document, so each distinct link is checked once rather than twenty
	// times: the probes are indexed point lookups, but forty of them on the
	// approval path is thirty-eight round trips that answer nothing new.
	checked := map[uuid.UUID]struct{}{}
	once := func(id *uuid.UUID, field string, exists func(context.Context, uuid.UUID, uuid.UUID) (bool, error)) error {
		if id == nil {
			return nil
		}
		if _, seen := checked[*id]; seen {
			return nil
		}
		if err := s.requireOwned(ctx, userID, *id, field, exists); err != nil {
			return err
		}
		checked[*id] = struct{}{}
		return nil
	}

	out := make([]CreateCardInput, 0, len(in))
	for _, card := range in {
		v, err := ValidateCard(NewCard{Front: card.Front, Back: card.Back})
		if err != nil {
			return nil, err
		}
		if err := once(card.StudyPlanID, "study_plan_id", s.store.PlanExists); err != nil {
			return nil, err
		}
		if err := once(card.DocumentID, "document_id", s.store.DocumentExists); err != nil {
			return nil, err
		}
		card.Front, card.Back = v.Front, v.Back
		out = append(out, card)
	}
	created, err := s.store.CreateFlashcards(ctx, userID, out)
	if err != nil {
		return nil, linkRace(err)
	}
	return created, nil
}

func (s *Service) DeleteFlashcard(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.DeleteFlashcard(ctx, userID, id)
}

// --- generation --------------------------------------------------------------------

// ProposeFlashcards reads a document and asks the model for cards about it.
//
// It writes nothing. What comes back is a Proposal -- cards, and the document
// and passages they were drawn from -- and storing them is a separate,
// approved call. That split is the whole approval story for this tool: the
// user reads the actual questions and answers before anything exists.
//
// The pipeline is: resolve the document, take the passages (the topic's, or
// the document's first few), prompt, parse leniently, then drop every card
// whose answer is not in those same passages. The last step is the one that
// makes the phase's claim true, and it is the reason this function returns how
// many cards it threw away.
func (s *Service) ProposeFlashcards(ctx context.Context, userID uuid.UUID, in GenerateInput) (Proposal, error) {
	in, err := ValidateGenerate(in)
	if err != nil {
		return Proposal{}, err
	}
	if s.model == nil || s.library == nil {
		return Proposal{}, fmt.Errorf("%w: no model or document library is wired", ErrGeneration)
	}
	if in.StudyPlanID != nil {
		if err := s.requireOwned(ctx, userID, *in.StudyPlanID, "study_plan_id", s.store.PlanExists); err != nil {
			return Proposal{}, err
		}
	}

	doc, err := s.library.Get(ctx, userID, in.DocumentID)
	if errors.Is(err, documents.ErrNotFound) {
		// The same 404 a foreign id gets everywhere else: another user's
		// document is indistinguishable from one that does not exist.
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, fmt.Errorf("read document: %w", err)
	}
	switch {
	case doc.Status != documents.StatusReady:
		return Proposal{}, fmt.Errorf("%w: %s is %s", ErrNotStudyable, doc.Filename, doc.Status)
	case doc.ChunkCount == 0:
		// Ready and empty: a scanned PDF whose text layer held nothing. The
		// status alone would read as "ready", which is true and unhelpful.
		return Proposal{}, fmt.Errorf("%w: nothing was extracted from %s", ErrNotStudyable, doc.Filename)
	}

	passages, err := s.passages(ctx, userID, in)
	if err != nil {
		return Proposal{}, err
	}
	shown := PromptPassages(passages)
	if len(shown) == 0 {
		return Proposal{}, ErrNoPassages
	}
	texts := PassageTexts(shown)

	cards, err := s.propose(ctx, shown, doc.Filename, in)
	if err != nil {
		return Proposal{}, err
	}

	kept := make([]NewCard, 0, len(cards))
	for _, c := range cards {
		if !CardGrounded(c, texts) {
			// Debug rather than warn: a model writing one unsupported card in
			// eight is the ordinary case this check exists for, not an
			// incident. The count reaches the caller in Proposal.Dropped.
			s.log.Debug("flashcard dropped: not grounded in the document",
				"user_id", userID, "document_id", in.DocumentID,
				"front", c.Front, "back", c.Back)
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return Proposal{}, fmt.Errorf("%w: the model proposed %d cards and none of them were in %s",
			ErrGeneration, len(cards), doc.Filename)
	}

	out := Proposal{
		DocumentID: doc.ID, Filename: doc.Filename, StudyPlanID: in.StudyPlanID,
		Topic: in.Topic, Cards: kept, Dropped: len(cards) - len(kept),
	}
	for _, p := range shown {
		out.ChunkIndexes = append(out.ChunkIndexes, p.ChunkIndex)
	}
	s.log.Debug("flashcards proposed",
		"user_id", userID, "document", doc.Filename, "topic", in.Topic,
		"proposed", len(cards), "kept", len(kept), "passages", len(shown))
	return out, nil
}

// passages picks the part of the document the cards come from.
//
// With a topic, that is the vector search restricted to this one document: the
// user said which part they wanted. With no topic it is the start of the
// document in order, which is not a guess at what they meant but the one
// answer that is the same every time -- and the proposal names the passages it
// used, so it is visible.
//
// A topic that matches nothing is ErrNoPassages rather than a silent fall back
// to the start of the document. Generating cards about chapter one because the
// user asked about a chapter that is not in the file would answer a question
// they did not ask, and they would have no way to see that it had happened.
func (s *Service) passages(ctx context.Context, userID uuid.UUID, in GenerateInput) ([]documents.Passage, error) {
	if in.Topic == "" {
		found, err := s.library.Passages(ctx, userID, in.DocumentID, PassagesPerGeneration)
		if err != nil {
			return nil, fmt.Errorf("read document passages: %w", err)
		}
		return found, nil
	}
	// No MinSimilarity: the search is already restricted to one document the
	// user named, so the floor that keeps an unrelated question from citing a
	// distant chunk has nothing to protect against here -- and applying the
	// chat floor would make "cards about the appendix" fail on a document
	// whose appendix is worded differently from the word "appendix".
	found, err := s.library.Search(ctx, userID, documents.SearchQuery{
		Query: in.Topic, Limit: PassagesPerGeneration, DocumentIDs: []uuid.UUID{in.DocumentID},
	})
	if err != nil {
		return nil, fmt.Errorf("search document for %q: %w", in.Topic, err)
	}
	if len(found) == 0 {
		return nil, ErrNoPassages
	}
	// Back into document order. The search returns them by similarity, and a
	// prompt whose passages jump around the document reads as a worse document
	// than the one the user uploaded.
	out := make([]documents.Passage, 0, len(found))
	for _, r := range found {
		out = append(out, documents.Passage{
			ChunkID: r.ChunkID, DocumentID: r.DocumentID, Filename: r.Filename,
			ChunkIndex: r.ChunkIndex, Content: r.Content,
		})
	}
	sortByIndex(out)
	return out, nil
}

// propose runs the model and turns its reply into candidate cards.
func (s *Service) propose(ctx context.Context, passages []documents.Passage, filename string, in GenerateInput) ([]NewCard, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	format := ""
	if s.opts.JSONMode {
		format = ai.FormatJSON
	}
	stream, err := s.model.Chat(ctx, GenerationPrompt(passages, filename, in.Topic, in.Count), ai.Options{
		Model:       s.opts.Model,
		Temperature: s.opts.Temperature,
		MaxTokens:   s.opts.MaxTokens,
		// Off by default; see Options.JSONMode. Either way the reply is parsed
		// defensively, because JSON asked for and JSON guaranteed are
		// different things and ParseFlashcards cannot tell which it has.
		Format: format,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGeneration, err)
	}
	defer stream.Close() //nolint:errcheck // releases the upstream connection

	reply, err := ai.Collect(stream)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGeneration, err)
	}
	cards := ParseFlashcards(reply, in.Count)
	if len(cards) == 0 {
		return nil, fmt.Errorf("%w: the model wrote no readable cards", ErrGeneration)
	}
	return cards, nil
}

// ResolveDocument finds the one document a reference means, among the caller's
// own documents only.
//
// By id if the reference is one; otherwise by filename, exactly
// (case-insensitively) and then as a substring, resolving only when exactly
// one document matches. Several matches are an error naming them, because
// guessing which file to generate from is exactly the decision the user should
// be making -- and a deck of cards from the wrong lecture is not something
// they will notice at the approval card.
//
// It is here rather than in the tool because it is the same owner-scoped
// lookup the rest of this service does, and because a later phase that lets a
// plan name its own document from the UI will want it too.
func (s *Service) ResolveDocument(ctx context.Context, userID uuid.UUID, ref string) (documents.Document, error) {
	if s.library == nil {
		return documents.Document{}, fmt.Errorf("%w: no document library is wired", ErrGeneration)
	}
	ref = strings.TrimSpace(strings.Trim(strings.TrimSpace(ref), `"'`))
	if ref == "" {
		return documents.Document{}, ErrNotFound
	}
	if id, err := uuid.Parse(ref); err == nil {
		doc, err := s.library.Get(ctx, userID, id)
		if errors.Is(err, documents.ErrNotFound) {
			return documents.Document{}, ErrNotFound
		}
		return doc, err
	}

	all, err := s.library.List(ctx, userID, documents.Filter{Limit: documents.MaxLimit, Sort: "filename"})
	if err != nil {
		return documents.Document{}, fmt.Errorf("list documents: %w", err)
	}
	var partial []documents.Document
	for _, d := range all {
		if strings.EqualFold(d.Filename, ref) {
			return d, nil
		}
		if strings.Contains(strings.ToLower(d.Filename), strings.ToLower(ref)) {
			partial = append(partial, d)
		}
	}
	switch len(partial) {
	case 1:
		return partial[0], nil
	case 0:
		return documents.Document{}, ErrNotFound
	}
	return documents.Document{}, &AmbiguousDocumentError{Ref: ref, Matches: filenames(partial)}
}

// AmbiguousDocumentError is a reference that names more than one of the
// caller's documents. It carries the names so the caller can ask a precise
// question rather than "which one?".
type AmbiguousDocumentError struct {
	Ref     string
	Matches []string
}

func (e *AmbiguousDocumentError) Error() string {
	return fmt.Sprintf("%q matches %s", e.Ref, strings.Join(e.Matches, ", "))
}

func filenames(docs []documents.Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Filename)
	}
	return out
}

// --- shared helpers ----------------------------------------------------------------

func (s *Service) requireOwnedDocument(ctx context.Context, userID uuid.UUID, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	return s.requireOwned(ctx, userID, *id, "document_id", s.store.DocumentExists)
}

// requireOwned checks a link to another row.
//
// A link to a row the caller does not own is reported as ErrNotFound -- a 404
// -- rather than as a validation error naming the field: the two answers
// differ, and only the first keeps another user's ids unconfirmable. It is the
// same rule calendar and finance apply to their links.
func (s *Service) requireOwned(ctx context.Context, userID, id uuid.UUID, field string,
	exists func(context.Context, uuid.UUID, uuid.UUID) (bool, error),
) error {
	if id == uuid.Nil {
		return validate.Errors{{Field: field, Message: "must be an id"}}
	}
	ok, err := exists(ctx, userID, id)
	if err != nil {
		return fmt.Errorf("check %s: %w", field, err)
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// linkRace turns the foreign-key violation a concurrent delete produces into
// the answer a missing link gets everywhere else.
//
// requireOwned has already checked that the caller owns the plan or document,
// but nothing holds it there: it can be deleted between the check and the
// write. The row it named is gone either way, so "no such study plan" is the
// honest answer, and it keeps a race from surfacing as a 500.
func linkRace(err error) error {
	if IsForeignKeyViolation(err) {
		return ErrNotFound
	}
	return err
}

// sortByIndex puts passages back in document order. It is an insertion sort
// over at most PassagesPerGeneration entries, which is smaller than the
// import.
func sortByIndex(ps []documents.Passage) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].ChunkIndex < ps[j-1].ChunkIndex; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
