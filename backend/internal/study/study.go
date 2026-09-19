// Package study owns the study module's first slice: study plans, and the
// flashcards generated from the user's own documents.
//
// It is the Phase 2 resource shape -- transport, service, repository, every
// query scoped to the owner -- with one thing in it that is not CRUD, and that
// one thing is the point of the phase: a flashcard generated from a document
// has to say what the document says.
//
// So generation is built as a retrieval problem with a model in the middle,
// not as a model answering from what it knows. The passages come out of
// document_chunks, which is text the user uploaded; they go into the prompt;
// the reply is parsed leniently (Phase 5/6's finding: strict JSON mode makes a
// 3B model measurably worse, see ParseFlashcards); and every proposed card is
// then checked against those same passages and dropped if its answer is not in
// them. A model that invents a plausible fact produces no card, rather than a
// card the user cannot tell apart from a real one. That check is
// grounding.go, and it is the inverse of the one internal/memories runs --
// memory rejects what came from a document, study requires it.
//
// The second thing worth naming up front is when generation happens. It runs
// while the call is being *prepared*, not when it is approved: the cards are
// part of the proposal the user reads, so the approval card shows the actual
// questions and answers rather than "create 8 flashcards". Approving stores
// exactly those cards. Nothing is regenerated at approval time -- that would
// write cards nobody had seen. See internal/tools/study_tools.go.
//
// What this slice deliberately is not. There are no quizzes (10b), no
// weak-topic tracking (10c), no spaced repetition (10d) and no sessions or
// streaks (10e). A flashcard here has a front, a back and a source; it has no
// review state, no due date and no "I got this right", and nothing in this
// package counts anything about how a card went. See docs/decisions.md.
package study

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/optional"
)

// ErrNotFound covers "no such study plan", "that plan is somebody else's", and
// a document link that is not the caller's. One error for all of them is what
// keeps the API from confirming foreign ids.
var ErrNotFound = errors.New("study plan not found")

// ErrFlashcardNotFound is the same answer one level down, kept separate only
// so the handler can say "flashcard" rather than "study plan" in the message.
// It is a 404 exactly like ErrNotFound.
var ErrFlashcardNotFound = errors.New("flashcard not found")

// ErrNotStudyable is a document that exists and has no text to generate from:
// still processing, failed, or a scanned PDF with no text layer. It is
// separate from ErrNotFound because the document *is* the caller's and saying
// so is useful -- "upload it again" is a different instruction from "there is
// no such document".
var ErrNotStudyable = errors.New("the document has no indexed text to study from")

// ErrNoPassages is a topic that matches nothing in the document. Generating
// from the whole document instead would answer a question the user did not
// ask, so the caller is told rather than quietly given something else.
var ErrNoPassages = errors.New("nothing in the document matches that topic")

// ErrGeneration classifies a failure to get usable cards out of the model: the
// call failed, the reply could not be parsed in any accepted shape, or nothing
// it proposed was grounded in the document. It is the study module's
// counterpart to memories.ErrExtraction.
var ErrGeneration = errors.New("flashcard generation failed")

// The statuses a plan can be in. A plan is `active` until the user says
// otherwise; `completed` is finished with, `abandoned` is dropped. They are
// validated in Go rather than by a CHECK constraint, for the reason migration
// 000010 gives.
const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusAbandoned = "abandoned"
)

// Statuses is the allow-list, used by validation and by the ?status= filter.
var Statuses = []string{StatusActive, StatusCompleted, StatusAbandoned}

// Plan mirrors a row of the study_plans table.
type Plan struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Title       string
	Description *string
	// DocumentID is the document the plan is built from, when it is built from
	// one, and *becomes* nil when that document is deleted: the plan is still
	// the plan.
	DocumentID *uuid.UUID
	// DocumentName is not a column -- it is joined on read, so a client and the
	// assistant can show "lecture-3.pdf" without a second lookup, and so a
	// deleted document reads back as no document rather than as a dangling id.
	DocumentName *string
	Status       string
	// CardCount is not a column either: it is counted on read, so it can never
	// disagree with the rows in flashcards. It is what makes a plan legible in
	// a list -- "Linear algebra, 24 cards" -- without a request per plan.
	CardCount int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Flashcard mirrors a row of the flashcards table.
//
// Front and Back, and nothing else. There is no ease factor, no interval, no
// due date and no counter of how it went: reviewing a card is 10c and 10d, and
// a column added now would be read by nothing for two phases. See
// docs/decisions.md.
type Flashcard struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// StudyPlanID is nil for a card that belongs to no plan.
	StudyPlanID *uuid.UUID
	// DocumentID is where the card's content came from, for a generated card,
	// and nil for one the user typed. It is the card's provenance and the
	// whole claim this phase makes about a generated card.
	DocumentID *uuid.UUID
	Front      string
	Back       string
	CreatedAt  time.Time
}

// CreatePlanInput is a validated new study plan.
type CreatePlanInput struct {
	Title       string
	Description string
	DocumentID  *uuid.UUID
	Status      string
}

// UpdatePlanInput is a PATCH body. An unset field is left alone; a field set
// to null clears the column.
type UpdatePlanInput struct {
	Title       optional.Field[string]
	Description optional.Field[string]
	DocumentID  optional.Field[uuid.UUID]
	Status      optional.Field[string]
}

// Patch is the normalized form of UpdatePlanInput handed to the repository.
type Patch struct {
	Title       *string
	Status      *string
	Description optional.Field[string]
	DocumentID  optional.Field[uuid.UUID]
}

// Empty reports whether the patch would touch no columns at all.
func (p Patch) Empty() bool {
	return p.Title == nil && p.Status == nil && !p.Description.Set && !p.DocumentID.Set
}

// NewCard is one front/back pair, before it is a row: what the model proposed,
// or what a user typed into the manual-add endpoint.
type NewCard struct {
	Front string
	Back  string
}

// CreateCardInput is a validated card ready to be written.
type CreateCardInput struct {
	StudyPlanID *uuid.UUID
	DocumentID  *uuid.UUID
	Front       string
	Back        string
}

// Filter is the query behind GET /study-plans.
type Filter struct {
	Status string
	// Query keeps the plans whose title or description contains it,
	// case-insensitively and literally -- `%` and `_` are characters, not
	// wildcards. Same contract as tasks.Filter.Query.
	Query string
	// DocumentID narrows to the plans built from one document.
	DocumentID *uuid.UUID
	Sort       string
	Limit      int
	Offset     int
}

// CardFilter is the query behind GET /study-plans/{id}/flashcards.
//
// There is no text search and no sort: a deck is read in the order it was
// made, and the only slice of it anybody asks for is "this plan's". Searching
// cards is what the quiz phase will want, and it can add the filter then.
type CardFilter struct {
	StudyPlanID *uuid.UUID
	Limit       int
	Offset      int
}

// Sorts maps the public `sort` values onto SQL.
var Sorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"updated_at":  "updated_at ASC",
	"-updated_at": "updated_at DESC",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

// DefaultSort is most recently made first, like notes and unlike the calendar:
// the plan you are working on is the one you made last.
const DefaultSort = "-created_at"

// Paging bounds for plans, as everywhere else.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Paging bounds for a plan's cards. They are larger than the resource
// defaults: a deck is meant to be read whole, and a client that has to page
// through 50-card windows to show one plan is a client that will just ask four
// times.
const (
	DefaultCardLimit = 200
	MaxCardLimit     = 500
)

// Field limits particular to this module. The shared ones live in
// internal/validate.
const (
	// MaxCardSideLen bounds one side of a card. A flashcard is a question and
	// an answer, not an essay: past this the card is not a card, and a
	// generated one that long is a model that started explaining itself.
	MaxCardSideLen = 1_000
	// MaxTopicLen bounds the topic a generation is narrowed to. It is a phrase
	// out of the user's message, and it is used as a retrieval query.
	MaxTopicLen = 200
)

// GenerateInput is a request to propose flashcards from one document.
type GenerateInput struct {
	DocumentID uuid.UUID
	// StudyPlanID is the plan the cards will be filed under, when there is
	// one. Generation does not create a plan.
	StudyPlanID *uuid.UUID
	// Topic narrows which passages of the document are used. Empty means the
	// start of the document, in order.
	Topic string
	// Count is how many cards to ask for, clamped to [1, MaxCardsPerBatch].
	Count int
}

// Card generation bounds.
const (
	// DefaultCardsPerBatch is what a request that names no number asks for.
	// Eight is a deck a person will actually read through in one sitting, and
	// it is about as many as llama3.2:3b writes before it starts repeating
	// itself with the words rearranged.
	DefaultCardsPerBatch = 8
	// MaxCardsPerBatch caps one generation. The prompt asks for Count; this is
	// the enforcement, because a prompt is a request and a cap is a guarantee
	// -- the same relationship memories.MaxFactsPerTurn has to its prompt.
	MaxCardsPerBatch = 20
	// PassagesPerGeneration is how much of the document goes into one prompt.
	// It is small on purpose: the cards have to be checkable against what the
	// model was shown, and a prompt of forty chunks is one nobody can check
	// and a 3B model does not read to the end of anyway.
	PassagesPerGeneration = 8
	// MaxGroundingChars truncates the passages shown to the model, in total.
	// A prompt that overflows the model's window is truncated by Ollama
	// silently, which is worse than truncating it here where the same text is
	// also what the grounding check runs against.
	MaxGroundingChars = 6_000
)

// Proposal is what one generation produced, before anything is stored.
//
// It carries the source alongside the cards because the source is the claim:
// these cards came from this document, and, for a topic-narrowed generation,
// from these passages of it. A client or a test can ask the same question the
// grounding check asked.
type Proposal struct {
	DocumentID   uuid.UUID
	Filename     string
	StudyPlanID  *uuid.UUID
	Topic        string
	Cards        []NewCard
	ChunkIndexes []int
	// Dropped is how many cards the model proposed that the grounding check
	// refused. It is reported rather than hidden: a generation that kept two
	// of eight is one whose document probably does not say what was asked.
	Dropped int
}
