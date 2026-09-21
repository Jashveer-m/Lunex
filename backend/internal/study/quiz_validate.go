package study

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// ValidateCreateQuiz checks and normalizes a whole quiz, reporting every
// problem at once.
//
// A quiz is validated as a unit because it is written as one: the questions
// are part of what the user approved, and half a quiz landing because the
// fourth question was malformed is a quiz nobody agreed to sit.
func ValidateCreateQuiz(in CreateQuizInput) (CreateQuizInput, error) {
	var errs validate.Errors

	title, e := validate.Title("title", in.Title)
	if e != nil {
		errs = append(errs, *e)
	}
	in.Title = title

	if e := linkID("study_plan_id", in.StudyPlanID); e != nil {
		errs = append(errs, *e)
	}
	if e := linkID("document_id", in.DocumentID); e != nil {
		errs = append(errs, *e)
	}

	switch {
	case len(in.Questions) == 0:
		errs = append(errs, validate.Error{Field: "questions", Message: "is required"})
	case len(in.Questions) > MaxQuestionsPerQuiz:
		errs = append(errs, validate.Error{
			Field:   "questions",
			Message: fmt.Sprintf("must be at most %d", MaxQuestionsPerQuiz),
		})
	}

	// The same question twice is a quiz that asks one thing twice and scores
	// it out of two. It is caught here rather than left to the model's
	// instructions, for the reason ParseFlashcards dedupes a repeated front.
	seen := make(map[string]struct{}, len(in.Questions))
	out := make([]NewQuestion, 0, len(in.Questions))
	for i, q := range in.Questions {
		v, err := ValidateQuestion(q)
		if err != nil {
			var qerrs validate.Errors
			if !errors.As(err, &qerrs) {
				return in, err
			}
			for _, e := range qerrs {
				errs = append(errs, validate.Error{
					Field:   fmt.Sprintf("questions[%d].%s", i, e.Field),
					Message: e.Message,
				})
			}
			continue
		}
		key := strings.ToLower(v.Question)
		if _, dup := seen[key]; dup {
			errs = append(errs, validate.Error{
				Field:   fmt.Sprintf("questions[%d].question", i),
				Message: "is already asked in this quiz",
			})
			continue
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	in.Questions = out
	return in, errs.OrNil()
}

// ValidateQuestion checks and normalizes one multiple-choice question,
// whichever side it came from.
//
// The rules are all one rule, which is that the question has to be answerable:
// something to ask, at least two distinct things to choose between, and a
// correct one among them. Every failure below is a question a person could be
// shown and could not answer.
func ValidateQuestion(q NewQuestion) (NewQuestion, error) {
	var errs validate.Errors

	q.Question = collapse(q.Question)
	switch {
	case q.Question == "":
		errs = append(errs, validate.Error{Field: "question", Message: "is required"})
	default:
		if e := validate.MaxLen("question", q.Question, MaxQuestionLen); e != nil {
			errs = append(errs, *e)
		}
	}

	// Options are normalized in place rather than compacted: correct_index
	// points at a position, so dropping a blank option would silently move the
	// answer to whatever came after it.
	options := make([]string, len(q.Options))
	distinct := make(map[string]struct{}, len(q.Options))
	for i, o := range q.Options {
		options[i] = collapse(o)
		switch {
		case options[i] == "":
			errs = append(errs, validate.Error{Field: "options", Message: "must not be blank"})
			continue
		default:
			if e := validate.MaxLen("options", options[i], MaxOptionLen); e != nil {
				errs = append(errs, *e)
			}
		}
		key := strings.ToLower(options[i])
		if _, dup := distinct[key]; dup {
			// Two identical choices make a question with two right answers or
			// two wrong ones, and no way for the taker to tell which they
			// picked.
			errs = append(errs, validate.Error{Field: "options", Message: "must not repeat a choice"})
			continue
		}
		distinct[key] = struct{}{}
	}
	q.Options = options
	switch {
	case len(options) < MinOptions:
		errs = append(errs, validate.Error{
			Field:   "options",
			Message: fmt.Sprintf("must have at least %d choices", MinOptions),
		})
	case len(options) > MaxOptions:
		errs = append(errs, validate.Error{
			Field:   "options",
			Message: fmt.Sprintf("must have at most %d choices", MaxOptions),
		})
	}

	if q.CorrectIndex < 0 || q.CorrectIndex >= len(options) {
		errs = append(errs, validate.Error{
			Field:   "correct_index",
			Message: "must be the position of one of the options",
		})
	}

	// The topic is optional and is never invented: a model that named none
	// leaves the column NULL rather than getting one derived from the
	// question, which would give 10c a tag that groups by wording.
	q.Topic = collapse(q.Topic)
	if e := validate.MaxLen("topic", q.Topic, MaxQuestionTopicLen); e != nil {
		errs = append(errs, *e)
	}
	return q, errs.OrNil()
}

// ValidateQuizFilter checks the query and clamps the paging.
func ValidateQuizFilter(f QuizFilter) (QuizFilter, error) {
	var errs validate.Errors

	if f.Sort == "" {
		f.Sort = DefaultQuizSort
	}
	if _, ok := QuizSorts[f.Sort]; !ok {
		errs = append(errs, validate.Error{Field: "sort", Message: "is not a sortable field"})
	}
	f.Query = strings.TrimSpace(f.Query)
	if e := validate.MaxLen("q", f.Query, validate.MaxQueryLen); e != nil {
		errs = append(errs, *e)
	}
	if e := linkID("study_plan_id", f.StudyPlanID); e != nil {
		errs = append(errs, *e)
	}
	if e := linkID("document_id", f.DocumentID); e != nil {
		errs = append(errs, *e)
	}
	switch {
	case f.Limit < 0:
		errs = append(errs, validate.Error{Field: "limit", Message: "must not be negative"})
	case f.Limit == 0:
		f.Limit = DefaultQuizLimit
	case f.Limit > MaxQuizLimit:
		f.Limit = MaxQuizLimit
	}
	if f.Offset < 0 {
		errs = append(errs, validate.Error{Field: "offset", Message: "must not be negative"})
	}
	return f, errs.OrNil()
}

// ValidateGenerateQuiz checks and normalizes a generation request, clamping
// the count rather than refusing it -- "quiz me on fifty questions" is a
// reasonable thing to say and MaxQuestionsPerQuiz is the honest answer to it.
func ValidateGenerateQuiz(in GenerateQuizInput) (GenerateQuizInput, error) {
	var errs validate.Errors

	if in.DocumentID == uuid.Nil {
		errs = append(errs, validate.Error{Field: "document_id", Message: "is required"})
	}
	if e := linkID("study_plan_id", in.StudyPlanID); e != nil {
		errs = append(errs, *e)
	}
	in.Topic = collapse(in.Topic)
	if e := validate.MaxLen("topic", in.Topic, MaxTopicLen); e != nil {
		errs = append(errs, *e)
	}
	in.Title = collapse(in.Title)
	if e := validate.MaxLen("title", in.Title, validate.MaxTitleLen); e != nil {
		errs = append(errs, *e)
	}
	switch {
	case in.Count < 0:
		errs = append(errs, validate.Error{Field: "count", Message: "must not be negative"})
	case in.Count == 0:
		in.Count = DefaultQuestionsPerQuiz
	case in.Count > MaxQuestionsPerQuiz:
		in.Count = MaxQuestionsPerQuiz
	}
	return in, errs.OrNil()
}

// ValidateAnswerInput checks a submitted answer as far as it can be checked
// without the question in front of it: an id, and an index that is an index.
// Whether the index is one of *that question's* options is the service's
// business, because only it has the question.
func ValidateAnswerInput(in AnswerInput) error {
	var errs validate.Errors
	if in.QuestionID == uuid.Nil {
		errs = append(errs, validate.Error{Field: "question_id", Message: "is required"})
	}
	if in.SelectedIndex < 0 {
		errs = append(errs, validate.Error{Field: "selected_index", Message: "must not be negative"})
	}
	return errs.OrNil()
}

// QuizTitleFor is the title a generation gets when the user named none.
//
// It is the document's name, narrowed by the topic when there is one. The
// filename rather than something the model wrote: a title is what the user
// picks the quiz out of a list by, and the one thing they certainly recognise
// is the file they uploaded.
func QuizTitleFor(filename, topic string) string {
	title := strings.TrimSpace(filename)
	if title == "" {
		title = "Quiz"
	}
	if topic != "" {
		title += " — " + topic
	}
	if len(title) > validate.MaxTitleLen {
		title = truncate(title, validate.MaxTitleLen)
	}
	return title
}
