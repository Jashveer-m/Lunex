package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/study"
)

// Phase 10c's one tool, and it is a read.
//
// get_weak_topics reports what the user's own quiz answers add up to, topic by
// topic, for the ones below the bar. It is analyze_spending's shape rather than
// a search's: there is no row that is the answer, so it is computed -- by the
// study service, from the answers -- and handed to the model already written
// out, so the model repeats numbers rather than producing them.
//
// It takes no arguments. The list is short by construction (only topics with
// enough evidence to be weak are in it), and an argument the router had to
// ground would be one more place for a guessed topic to empty the result and
// read as "you have no weak topics".

// weakTopicRecord is one weak topic as the tool reports it.
type weakTopicRecord struct {
	Topic          string   `json:"topic"`
	Answers        int      `json:"answers"`
	Correct        int      `json:"correct"`
	CorrectPercent int      `json:"correct_percent"`
	Quizzes        int      `json:"quizzes"`
	StudyPlans     []string `json:"study_plans"`
	Documents      []string `json:"documents"`
	LastAnswered   string   `json:"last_answered"`
}

func toWeakTopicRecord(t study.TopicStat) weakTopicRecord {
	plans, docs := t.StudyPlans, t.Documents
	if plans == nil {
		plans = []string{}
	}
	if docs == nil {
		docs = []string{}
	}
	return weakTopicRecord{
		Topic: t.Topic, Answers: t.Answers, Correct: t.Correct,
		CorrectPercent: t.CorrectPercent(), Quizzes: t.Quizzes,
		StudyPlans: plans, Documents: docs,
		LastAnswered: t.LastAnswered.UTC().Format(time.DateOnly),
	}
}

type getWeakTopicsInput struct{}

func getWeakTopicsTool(s Services) Tool {
	return define(Tool{
		Name: GetWeakTopics,
		Description: "Look at which quiz topics the user keeps getting wrong, with how many times they answered " +
			"and how many they got right. Use it for how they are doing in their quizzes, what they are weak at, " +
			"or what they should study or revise.",
		Permission: Read,
		Output: object(map[string]Schema{
			"count":               integer("how many weak topics are listed"),
			"min_answers":         integer("how many answers a topic needs before it can be weak"),
			"max_correct_percent": integer("a topic is weak when fewer than this percent of its answers are correct"),
			"weak_topics": listOf(object(map[string]Schema{
				"topic":           str("the topic, as the quiz questions name it"),
				"answers":         integer("how many times a question on it was answered"),
				"correct":         integer("how many of those answers were right"),
				"correct_percent": integer("correct as a whole percentage of answers"),
				"quizzes":         integer("how many quizzes the answers came from"),
				"study_plans":     listOf(str("a study plan those quizzes are filed under")),
				"documents":       listOf(str("a document those quizzes were made from")),
				"last_answered":   str("the day a question on it was last answered"),
			}, "topic", "answers", "correct", "correct_percent", "quizzes", "study_plans", "documents", "last_answered")),
		}, "count", "min_answers", "max_correct_percent", "weak_topics"),
	},
		func(context.Context, uuid.UUID, Args) (getWeakTopicsInput, error) {
			return getWeakTopicsInput{}, nil
		},
		func(ctx context.Context, userID uuid.UUID, _ getWeakTopicsInput) (Result, error) {
			found, err := s.Study.WeakTopics(ctx, userID)
			if err != nil {
				return Result{}, fmt.Errorf("get weak topics: %w", err)
			}
			records := make([]weakTopicRecord, 0, len(found))
			for _, t := range found {
				records = append(records, toWeakTopicRecord(t))
			}
			return Result{
				Output: map[string]any{
					"count": len(records), "weak_topics": records,
					"min_answers":         study.WeakTopicMinAnswers,
					"max_correct_percent": int(study.WeakTopicMaxCorrectRate * 100),
				},
				WeakTopics: found,
			}, nil
		},
		func(getWeakTopicsInput) string {
			return "Look at which quiz topics are weakest."
		},
	)
}
