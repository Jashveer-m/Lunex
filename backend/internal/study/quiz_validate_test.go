package study

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/validate"
)

// fields is the field names a validation error names, in order.
func quizFields(t *testing.T, err error) []string {
	t.Helper()
	var errs validate.Errors
	if !errors.As(err, &errs) {
		t.Fatalf("err = %v, want validate.Errors", err)
	}
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Field)
	}
	return out
}

// Every rule on a question is the same rule -- it has to be answerable -- and
// each of these is a question a person could be shown and could not answer.
func TestValidateQuestionRequiresAnAnswerableQuestion(t *testing.T) {
	ok := NewQuestion{
		Question: "How long does the changeover take?",
		Options:  []string{"Eleven seconds", "Two minutes"}, CorrectIndex: 0,
	}
	if _, err := ValidateQuestion(ok); err != nil {
		t.Fatalf("a good question was refused: %v", err)
	}

	for name, tc := range map[string]struct {
		in    NewQuestion
		field string
	}{
		"no question": {NewQuestion{Options: []string{"a", "b"}}, "question"},
		"one option": {NewQuestion{
			Question: "q?", Options: []string{"a"},
		}, "options"},
		"seven options": {NewQuestion{
			Question: "q?", Options: []string{"a", "b", "c", "d", "e", "f", "g"},
		}, "options"},
		"a blank option": {NewQuestion{
			Question: "q?", Options: []string{"a", "   "},
		}, "options"},
		"the same choice twice": {NewQuestion{
			Question: "q?", Options: []string{"Eleven seconds", "eleven  seconds"},
		}, "options"},
		"an answer that is not an option": {NewQuestion{
			Question: "q?", Options: []string{"a", "b"}, CorrectIndex: 5,
		}, "correct_index"},
		"a negative answer": {NewQuestion{
			Question: "q?", Options: []string{"a", "b"}, CorrectIndex: -1,
		}, "correct_index"},
		"an overlong topic": {NewQuestion{
			Question: "q?", Options: []string{"a", "b"},
			Topic: strings.Repeat("x", MaxQuestionTopicLen+1),
		}, "topic"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateQuestion(tc.in)
			fields := quizFields(t, err)
			for _, f := range fields {
				if f == tc.field {
					return
				}
			}
			t.Fatalf("fields = %v, want one of them to be %q", fields, tc.field)
		})
	}
}

// Options are normalized in place rather than compacted: correct_index points
// at a position, so dropping one would move the answer.
func TestValidateQuestionNormalizesWithoutMovingTheAnswer(t *testing.T) {
	q, err := ValidateQuestion(NewQuestion{
		Question:     "  How   long does it\ntake? ",
		Options:      []string{" Ten  seconds ", "Eleven\tseconds"},
		CorrectIndex: 1, Topic: "  changeover ",
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case q.Question != "How long does it take?":
		t.Fatalf("question = %q", q.Question)
	case strings.Join(q.Options, "|") != "Ten seconds|Eleven seconds":
		t.Fatalf("options = %v", q.Options)
	case q.CorrectAnswer() != "Eleven seconds":
		t.Fatalf("the answer moved: %q", q.CorrectAnswer())
	case q.Topic != "changeover":
		t.Fatalf("topic = %q", q.Topic)
	}
}

// A whole quiz reports every problem at once, and says which question each one
// is in -- a quiz is validated as a unit because it is written as one.
func TestValidateCreateQuizNamesTheQuestionAProblemIsIn(t *testing.T) {
	_, err := ValidateCreateQuiz(CreateQuizInput{
		Title: "Relay handbook",
		Questions: []NewQuestion{
			{Question: "Fine?", Options: []string{"a", "b"}, CorrectIndex: 0},
			{Question: "Bad?", Options: []string{"a"}, CorrectIndex: 9},
		},
	})
	fields := quizFields(t, err)
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want both problems reported", fields)
	}
	for _, f := range fields {
		if !strings.HasPrefix(f, "questions[1].") {
			t.Fatalf("field %q does not say which question it is in", f)
		}
	}
}

func TestValidateCreateQuizRefusesADuplicateQuestion(t *testing.T) {
	_, err := ValidateCreateQuiz(CreateQuizInput{
		Title: "Relay handbook",
		Questions: []NewQuestion{
			{Question: "How long?", Options: []string{"a", "b"}, CorrectIndex: 0},
			{Question: "how  long?", Options: []string{"c", "d"}, CorrectIndex: 1},
		},
	})
	if fields := quizFields(t, err); fields[0] != "questions[1].question" {
		t.Fatalf("fields = %v", fields)
	}
}

func TestValidateCreateQuizBoundsTheQuiz(t *testing.T) {
	one := NewQuestion{Question: "q?", Options: []string{"a", "b"}, CorrectIndex: 0}
	zero := uuid.Nil
	for name, tc := range map[string]struct {
		in    CreateQuizInput
		field string
	}{
		"no title":     {CreateQuizInput{Questions: []NewQuestion{one}}, "title"},
		"no questions": {CreateQuizInput{Title: "Empty"}, "questions"},
		"too many questions": {CreateQuizInput{
			Title: "Long", Questions: distinctQuestions(MaxQuestionsPerQuiz + 1),
		}, "questions"},
		"a zero plan id": {CreateQuizInput{
			Title: "Zero", StudyPlanID: &zero, Questions: []NewQuestion{one},
		}, "study_plan_id"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ValidateCreateQuiz(tc.in)
			if fields := quizFields(t, err); fields[0] != tc.field {
				t.Fatalf("fields = %v, want %q", fields, tc.field)
			}
		})
	}
}

// "Quiz me on fifty questions" is a reasonable thing to say, and
// MaxQuestionsPerQuiz is the honest answer to it.
func TestValidateGenerateQuizClampsTheCount(t *testing.T) {
	for in, want := range map[int]int{
		0: DefaultQuestionsPerQuiz, 3: 3, 50: MaxQuestionsPerQuiz,
	} {
		got, err := ValidateGenerateQuiz(GenerateQuizInput{DocumentID: uuid.New(), Count: in})
		if err != nil {
			t.Fatal(err)
		}
		if got.Count != want {
			t.Fatalf("count %d became %d, want %d", in, got.Count, want)
		}
	}
	if _, err := ValidateGenerateQuiz(GenerateQuizInput{Count: -1}); err != nil {
		if fields := quizFields(t, err); fields[0] != "document_id" || fields[1] != "count" {
			t.Fatalf("fields = %v", fields)
		}
	} else {
		t.Fatal("a negative count and no document were accepted")
	}
}

func TestQuizTitleForNamesTheFileAndTheTopic(t *testing.T) {
	for _, tc := range []struct{ filename, topic, want string }{
		{"relay-handbook.txt", "", "relay-handbook.txt"},
		{"relay-handbook.txt", "the battery bank", "relay-handbook.txt — the battery bank"},
		{"", "", "Quiz"},
	} {
		if got := QuizTitleFor(tc.filename, tc.topic); got != tc.want {
			t.Fatalf("QuizTitleFor(%q, %q) = %q, want %q", tc.filename, tc.topic, got, tc.want)
		}
	}
}

// distinctQuestions is n questions that differ, so a length check is not
// confused with the duplicate check.
func distinctQuestions(n int) []NewQuestion {
	out := make([]NewQuestion, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, NewQuestion{
			Question: "Question " + string(rune('a'+i)) + "?",
			Options:  []string{"a", "b"}, CorrectIndex: 0,
		})
	}
	return out
}
