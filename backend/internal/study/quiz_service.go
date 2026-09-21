package study

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// The quiz use cases. They split cleanly in two, and the split is the phase's
// one design decision worth stating up front.
//
// Making a quiz is the assistant proposing content: ProposeQuiz writes
// nothing, CreateQuiz stores exactly what was proposed, and the two are
// separate calls so the user reads the questions before they exist. That is
// the flashcard story unchanged.
//
// Taking one is not that at all. StartAttempt, SubmitAnswer and
// CompleteAttempt are the user answering their own questions -- there is no
// proposal, no approval and no tool that can reach them, because there is
// nothing for the user to be protected from. Approval stands between the
// assistant and the user's data, not between the user and their own.

// --- quizzes --------------------------------------------------------------------

// Quizzes lists the caller's quizzes. The questions are not on them; see
// Repository.Quizzes.
func (s *Service) Quizzes(ctx context.Context, userID uuid.UUID, f QuizFilter) ([]Quiz, error) {
	f, err := ValidateQuizFilter(f)
	if err != nil {
		return nil, err
	}
	return s.store.Quizzes(ctx, userID, f)
}

// Quiz returns one quiz with its questions.
func (s *Service) Quiz(ctx context.Context, userID, id uuid.UUID) (Quiz, error) {
	return s.store.QuizByID(ctx, userID, id)
}

// CreateQuiz writes a quiz and its questions, checking every link first.
//
// This is what an approved generation runs. It takes the questions rather than
// a document and a count, for the reason CreateFlashcards takes cards: the
// questions were written when the proposal was made and are what the user
// approved, and generating again here would store a quiz nobody had read.
func (s *Service) CreateQuiz(ctx context.Context, userID uuid.UUID, in CreateQuizInput) (Quiz, error) {
	in, err := ValidateCreateQuiz(in)
	if err != nil {
		return Quiz{}, err
	}
	if in.StudyPlanID != nil {
		if err := s.requireOwned(ctx, userID, *in.StudyPlanID, "study_plan_id", s.store.PlanExists); err != nil {
			return Quiz{}, quizLinkRace(err)
		}
	}
	if err := s.requireOwnedDocument(ctx, userID, in.DocumentID); err != nil {
		return Quiz{}, quizLinkRace(err)
	}
	created, err := s.store.CreateQuiz(ctx, userID, in)
	if err != nil {
		return Quiz{}, quizLinkRace(err)
	}
	return created, nil
}

// DeleteQuiz removes a quiz and, through the cascade, its questions, its
// attempts and their answers.
//
// It makes no graph call because a quiz has no node; see migration 000011.
func (s *Service) DeleteQuiz(ctx context.Context, userID, id uuid.UUID) error {
	return s.store.DeleteQuiz(ctx, userID, id)
}

// --- attempts -------------------------------------------------------------------

// StartAttempt opens an attempt at one of the caller's quizzes.
//
// Nothing stops a user starting a second attempt while the first is open, and
// that is deliberate: an attempt is a row, retaking a quiz is a new row, and a
// rule that there may be only one open attempt would need an answer to "what
// happens to the old one" that nobody has asked for.
func (s *Service) StartAttempt(ctx context.Context, userID, quizID uuid.UUID) (Attempt, error) {
	return s.store.CreateAttempt(ctx, userID, quizID)
}

// Attempt returns one attempt with the answers given so far and, once it is
// finished, its score.
func (s *Service) Attempt(ctx context.Context, userID, id uuid.UUID) (Attempt, error) {
	return s.store.AttemptByID(ctx, userID, id)
}

// SubmitAnswer records and grades one answer.
//
// Grading happens here, when the answer is given, rather than being worked out
// again whenever the attempt is read. That is what makes an attempt a record
// of what happened: the verdict is stored beside the choice, and a question
// deleted afterwards takes its answers with it rather than leaving a row that
// would now be scored differently.
//
// The four ways this can fail are four different answers, and telling them
// apart is the point of doing the reads before the write:
//
//   - the attempt is not the caller's, or does not exist: 404.
//   - it is finished: 409. The score is written, and a thirteenth answer to a
//     twelve-question attempt scored 9 would make the stored score disagree
//     with the rows it came from.
//   - the question is not one of this attempt's quiz's: 404. That includes a
//     question of the caller's *own* other quiz, which is a real mistake a
//     client makes and not a leak.
//   - the question was already answered in this attempt: 409, not an
//     overwrite. See ErrAlreadyAnswered.
func (s *Service) SubmitAnswer(ctx context.Context, userID, attemptID uuid.UUID, in AnswerInput) (Answer, error) {
	if err := ValidateAnswerInput(in); err != nil {
		return Answer{}, err
	}
	attempt, err := s.store.AttemptByID(ctx, userID, attemptID)
	if err != nil {
		return Answer{}, err
	}
	if attempt.Complete() {
		return Answer{}, ErrAttemptComplete
	}
	question, err := s.store.QuestionForAttempt(ctx, userID, attemptID, in.QuestionID)
	if err != nil {
		return Answer{}, err
	}
	if in.SelectedIndex >= len(question.Options) {
		// A field error rather than a 404: the question is real and the
		// caller's, and "there is no option 7" is a useful thing to be told.
		return Answer{}, validate.Errors{{
			Field:   "selected_index",
			Message: fmt.Sprintf("must be the position of one of the %d options", len(question.Options)),
		}}
	}
	for _, a := range attempt.Answers {
		if a.QuestionID == in.QuestionID {
			return Answer{}, ErrAlreadyAnswered
		}
	}
	// The verdict. It is computed here and stored, and nothing recomputes it.
	answer, err := s.store.CreateAnswer(ctx, userID, attemptID, in, in.SelectedIndex == question.CorrectIndex)
	if err != nil {
		return Answer{}, err
	}
	return answer, nil
}

// CompleteAttempt finalises an attempt and scores it out of the questions
// answered correctly.
//
// An attempt with questions left unanswered can still be completed: they are
// not counted as wrong, they are not counted at all, and the attempt carries
// the quiz's question count so the score reads as "4 of 6". Refusing to close
// an unfinished attempt would leave a user who stopped halfway with a row that
// can never be anything else.
func (s *Service) CompleteAttempt(ctx context.Context, userID, id uuid.UUID) (Attempt, error) {
	return s.store.CompleteAttempt(ctx, userID, id)
}

// --- generation -----------------------------------------------------------------

// ProposeQuiz reads a document and asks the model for multiple-choice
// questions about it.
//
// It writes nothing. What comes back is a QuizProposal -- the questions, their
// options, which option is right, and the document and passages they were
// drawn from -- and storing it is a separate, approved call. That split is the
// whole approval story for this tool, exactly as it is for flashcards: the
// user reads the actual questions before anything exists.
//
// The pipeline is ProposeFlashcards' with one step doing a different check:
// resolve the document, take the passages, prompt, parse leniently, then drop
// every question whose *correct answer* is not in those same passages. A wrong
// distractor is fine -- it is supposed to be wrong -- and is not checked at
// all; see QuestionGrounded for what that costs and why it is still right.
func (s *Service) ProposeQuiz(ctx context.Context, userID uuid.UUID, in GenerateQuizInput) (QuizProposal, error) {
	in, err := ValidateGenerateQuiz(in)
	if err != nil {
		return QuizProposal{}, err
	}
	if s.model == nil || s.library == nil {
		return QuizProposal{}, fmt.Errorf("%w: no model or document library is wired", ErrGeneration)
	}
	if in.StudyPlanID != nil {
		if err := s.requireOwned(ctx, userID, *in.StudyPlanID, "study_plan_id", s.store.PlanExists); err != nil {
			return QuizProposal{}, err
		}
	}

	doc, shown, err := s.generationSource(ctx, userID, in.DocumentID, in.Topic)
	if err != nil {
		return QuizProposal{}, err
	}
	texts := PassageTexts(shown)

	reply, err := s.generate(ctx, QuizGenerationPrompt(shown, doc.Filename, in.Topic, in.Count))
	if err != nil {
		return QuizProposal{}, err
	}
	questions := ParseQuizQuestions(reply, in.Count)
	if len(questions) == 0 {
		return QuizProposal{}, fmt.Errorf("%w: the model wrote no readable questions", ErrGeneration)
	}

	kept := make([]NewQuestion, 0, len(questions))
	for _, q := range questions {
		if !QuestionGrounded(q, texts) {
			// Debug rather than warn: a model writing one unsupported answer
			// in six is the ordinary case this check exists for, not an
			// incident. The count reaches the caller in QuizProposal.Dropped.
			s.log.Debug("quiz question dropped: its answer is not in the document",
				"user_id", userID, "document_id", in.DocumentID,
				"question", q.Question, "answer", q.CorrectAnswer())
			continue
		}
		kept = append(kept, q)
	}
	if len(kept) == 0 {
		return QuizProposal{}, fmt.Errorf("%w: the model proposed %d questions and none of their answers were in %s",
			ErrGeneration, len(questions), doc.Filename)
	}

	title := in.Title
	if title == "" {
		title = QuizTitleFor(doc.Filename, in.Topic)
	}
	out := QuizProposal{
		DocumentID: doc.ID, Filename: doc.Filename, StudyPlanID: in.StudyPlanID,
		Topic: in.Topic, Title: title, Questions: kept, Dropped: len(questions) - len(kept),
	}
	for _, p := range shown {
		out.ChunkIndexes = append(out.ChunkIndexes, p.ChunkIndex)
	}
	s.log.Debug("quiz proposed",
		"user_id", userID, "document", doc.Filename, "topic", in.Topic,
		"proposed", len(questions), "kept", len(kept), "passages", len(shown))
	return out, nil
}

// quizLinkRace turns the "no such study plan" a link check produces into the
// quiz module's own 404, and a foreign-key violation from a concurrent delete
// into the same thing.
//
// The two errors are one answer on this path. A quiz whose plan or document
// vanished between the check and the write names a row that is gone, and the
// caller asked about a quiz -- so "no such quiz" is what they are told, rather
// than a 500 or an error naming a resource they did not mention.
func quizLinkRace(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), IsForeignKeyViolation(err):
		return ErrQuizNotFound
	}
	return err
}
