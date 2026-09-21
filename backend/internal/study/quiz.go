package study

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// The study module's second slice: quizzes.
//
// A quiz is a title and a list of multiple-choice questions written from the
// user's own document, on exactly the terms a flashcard is written on --
// generated while the tool call is *prepared*, checked against the passages
// before anybody sees it, and stored only when the user approves the proposal
// they read. The one new rule is which part of a question has to be grounded:
// the correct answer must trace to the text, and a distractor is supposed to
// be wrong, so it is not checked. See QuestionGrounded in grounding.go.
//
// Taking a quiz is the other half, and it is not an approval flow at all. An
// attempt is the user answering their own questions: there is no tool that
// submits an answer, and the endpoints that grade one are as direct as POST
// /study-plans/{id}/flashcards is. Approval stands between the assistant and
// the user's data, not between the user and their own.
//
// What is deliberately absent. Nothing here aggregates `topic` across attempts
// -- that is 10c, and this phase's job is to record the evidence it will read:
// a topic per question and a verdict per answer. There is no due date and no
// interval (10d), and no session or streak (10e).

// ErrQuizNotFound covers "no such quiz", "that quiz is somebody else's", and a
// study plan or document link that is not the caller's. One error for all of
// them is what keeps the API from confirming foreign ids.
var ErrQuizNotFound = errors.New("quiz not found")

// ErrAttemptNotFound is the same answer for an attempt.
var ErrAttemptNotFound = errors.New("quiz attempt not found")

// ErrQuestionNotFound is an answer submitted for a question that is not in the
// attempt's quiz -- including one that is in *another* quiz of the caller's,
// which is a real mistake a client can make and not a leak. It is a 404 like
// the rest.
var ErrQuestionNotFound = errors.New("quiz question not found")

// ErrAlreadyAnswered is the second answer to one question in one attempt.
//
// It is a conflict rather than an overwrite: an attempt is a record of what
// the user actually answered, and letting the second submission win would make
// the score a record of what they answered last, having seen the first verdict.
// Retaking the quiz is a new attempt, which is a new row.
var ErrAlreadyAnswered = errors.New("that question has already been answered in this attempt")

// ErrAttemptComplete is an answer submitted to, or a second completion of, an
// attempt that is already finished. Also a conflict: the score is written, and
// a thirteenth answer to a twelve-question attempt scored 9 would make the
// stored score disagree with the rows it was computed from.
var ErrAttemptComplete = errors.New("this attempt is already complete")

// Quiz mirrors a row of the quizzes table, with what a read joins onto it.
type Quiz struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// StudyPlanID is the plan the quiz is filed under, when there is one, and
	// the quiz goes with the plan if the plan is deleted.
	StudyPlanID *uuid.UUID
	// StudyPlanTitle is not a column: it is joined on read, so a client and
	// the assistant can say "under Kestrel relay handbook" without a second
	// lookup.
	StudyPlanTitle *string
	// DocumentID is where the questions came from, and *becomes* nil when that
	// document is deleted: the quiz and its attempts are still what happened.
	DocumentID *uuid.UUID
	// DocumentName is joined on read, like Plan.DocumentName.
	DocumentName *string
	Title        string
	// QuestionCount, AttemptCount and BestScore are counted on read rather
	// than stored, for the reason Plan.CardCount is: a counter in a column is
	// a counter that can be wrong, and these cannot. Together they are what
	// makes a quiz legible in a list -- "Relay handbook, 6 questions, best 5"
	// -- without a request per quiz.
	QuestionCount int
	AttemptCount  int
	// BestScore is the highest score over the caller's *completed* attempts,
	// or nil if they have not finished one. It is a maximum over one quiz's
	// attempts and nothing else: the weak-topic analysis across quizzes is
	// 10c, and this is not a first instalment of it.
	BestScore *int
	CreatedAt time.Time
	// Questions is filled by the single-quiz read and empty in a list. The
	// correct answer is not on it -- see Question.CorrectIndex.
	Questions []Question
}

// Question mirrors a row of quiz_questions.
type Question struct {
	ID       uuid.UUID
	QuizID   uuid.UUID
	Question string
	// Options are the choices, in the order they are shown. The order is the
	// model's and is never shuffled: a proposal the user approved has to write
	// exactly the quiz they read, and a quiz that rearranged itself between
	// the approval card and the database is one they did not approve.
	Options []string
	// CorrectIndex is an index into Options. It is loaded on every read the
	// service does -- grading needs it -- and is deliberately *not* on the
	// wire shape of a quiz; see quiz_handlers.go.
	CorrectIndex int
	// Topic is a short tag for what the question is about, or nil. Nothing in
	// this phase reads it.
	Topic     *string
	CreatedAt time.Time
}

// Attempt mirrors a row of quiz_attempts, with what a read joins onto it.
type Attempt struct {
	ID     uuid.UUID
	UserID uuid.UUID
	QuizID uuid.UUID
	// QuizTitle is joined on read, so an attempt is legible on its own.
	QuizTitle string
	// QuestionCount is the quiz's question count, counted on read. It is what
	// makes Score legible: "4" means nothing and "4 out of 6" does.
	QuestionCount int
	StartedAt     time.Time
	// CompletedAt and Score are nil together, and set together, by Complete.
	CompletedAt *time.Time
	Score       *int
	// Answers is filled by the single-attempt read and empty in a list.
	Answers []Answer
}

// Complete reports whether the attempt has been finalised.
func (a Attempt) Complete() bool { return a.CompletedAt != nil }

// Answer mirrors a row of quiz_answers, with the question it answers joined
// on.
//
// The question's text and its correct index are carried because the only place
// an answer is read is a review of an attempt, and "you picked 2" is not a
// review. They are safe to show here in a way they are not on the quiz itself:
// this row exists because the user already answered that question.
type Answer struct {
	ID            uuid.UUID
	AttemptID     uuid.UUID
	QuestionID    uuid.UUID
	Question      string
	Options       []string
	SelectedIndex int
	CorrectIndex  int
	Correct       bool
	CreatedAt     time.Time
}

// NewQuestion is one proposed question, before it is a row: what the model
// wrote, or what an approved proposal carries.
type NewQuestion struct {
	Question     string
	Options      []string
	CorrectIndex int
	Topic        string
}

// CorrectAnswer is the option the question says is right. It is the string the
// grounding check runs against, and it is a method rather than a field so the
// index and the text cannot drift apart.
func (q NewQuestion) CorrectAnswer() string {
	if q.CorrectIndex < 0 || q.CorrectIndex >= len(q.Options) {
		return ""
	}
	return q.Options[q.CorrectIndex]
}

// CreateQuizInput is a validated quiz ready to be written, questions and all.
//
// A quiz is created whole. There is no endpoint that adds a question to an
// existing quiz, because a quiz with a question appended is a different quiz
// from the one somebody has already sat -- and the attempts of the old one
// would silently be scored out of the new total.
type CreateQuizInput struct {
	StudyPlanID *uuid.UUID
	DocumentID  *uuid.UUID
	Title       string
	Questions   []NewQuestion
}

// AnswerInput is one submitted answer.
type AnswerInput struct {
	QuestionID    uuid.UUID
	SelectedIndex int
}

// QuizFilter is the query behind GET /quizzes.
type QuizFilter struct {
	// Query keeps the quizzes whose title contains it, case-insensitively and
	// literally -- `%` and `_` are characters, not wildcards.
	Query string
	// StudyPlanID narrows to one plan's quizzes, DocumentID to one document's.
	StudyPlanID *uuid.UUID
	DocumentID  *uuid.UUID
	Sort        string
	Limit       int
	Offset      int
}

// QuizSorts maps the public `sort` values onto SQL. There is no updated_at: a
// quiz is written once and never edited, so the column does not exist.
var QuizSorts = map[string]string{
	"created_at":  "created_at ASC",
	"-created_at": "created_at DESC",
	"title":       "lower(title) ASC",
	"-title":      "lower(title) DESC",
}

// DefaultQuizSort is most recently made first, like plans and notes.
const DefaultQuizSort = "-created_at"

// Paging bounds for quizzes. The resource defaults, not the deck ones: a quiz
// list is a list of titles, and unlike a deck it is not meant to be read whole.
const (
	DefaultQuizLimit = 50
	MaxQuizLimit     = 200
)

// Field limits particular to quizzes.
const (
	// MaxQuestionLen bounds the question text. It is the card limit: a
	// question longer than this is a model explaining the passage.
	MaxQuestionLen = 1_000
	// MaxOptionLen bounds one option. Shorter than a question on purpose -- an
	// option is an answer, and four paragraph-long choices are not a
	// multiple-choice question.
	MaxOptionLen = 500
	// MaxQuestionTopicLen bounds the per-question tag. It is a phrase, and 10c
	// will group by it; something the length of a sentence would group with
	// nothing.
	MaxQuestionTopicLen = 100
	// MinOptions and MaxOptions bound one question's choices. Two is the
	// fewest that is a choice at all; six is as many as a person reads before
	// the question becomes a reading test. Four is what the prompt asks for.
	MinOptions = 2
	MaxOptions = 6
)

// Quiz generation bounds, the counterparts of the flashcard ones.
const (
	// DefaultQuestionsPerQuiz is what a request that names no number asks for.
	// Six is a quiz somebody sits in one go, and -- like eight cards -- about
	// as many as a 3B model writes from eight passages before it starts asking
	// the same question with the words rearranged.
	DefaultQuestionsPerQuiz = 6
	// MaxQuestionsPerQuiz caps one generation, and one quiz. The prompt asks
	// for Count; this is the enforcement, because a prompt is a request and a
	// cap is a guarantee.
	MaxQuestionsPerQuiz = 15
	// OptionsPerQuestion is how many choices the prompt asks for. It is not a
	// validation bound -- that is MinOptions/MaxOptions -- because a model
	// that writes three good options should not lose the question over it.
	OptionsPerQuestion = 4
)

// GenerateQuizInput is a request to propose a quiz from one document.
type GenerateQuizInput struct {
	DocumentID uuid.UUID
	// StudyPlanID is the plan the quiz will be filed under, when there is one.
	// Generation does not create a plan.
	StudyPlanID *uuid.UUID
	// Topic narrows which passages of the document are used. Empty means the
	// start of the document, in order.
	Topic string
	// Count is how many questions to ask for, clamped to
	// [1, MaxQuestionsPerQuiz].
	Count int
	// Title is what the quiz will be called. Empty means one derived from the
	// document and the topic; see ValidateGenerateQuiz.
	Title string
}

// QuizProposal is what one generation produced, before anything is stored.
//
// Like Proposal it carries the source alongside the questions, because the
// source is the claim: these questions came from this document, and, for a
// topic-narrowed generation, from these passages of it.
type QuizProposal struct {
	DocumentID   uuid.UUID
	Filename     string
	StudyPlanID  *uuid.UUID
	Topic        string
	Title        string
	Questions    []NewQuestion
	ChunkIndexes []int
	// Dropped is how many questions the model proposed that the checks
	// refused: malformed, duplicated, or with a correct answer the passages do
	// not support. It is reported rather than hidden, exactly as
	// Proposal.Dropped is.
	Dropped int
}
