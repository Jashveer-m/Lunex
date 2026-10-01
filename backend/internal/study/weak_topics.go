package study

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/jashveer/lifeos/backend/internal/db"
)

// The study module's third slice: weak topics.
//
// There is no table here and no migration. 10b recorded the two facts this
// needs -- a topic per question and a verdict per answer -- and this phase is
// the GROUP BY over them. A weak topic is a computation, not a record: it is
// worked out from the answers on every read, so it cannot drift from them, and
// a topic that stops being weak because the user practised it stops being
// reported without anything having to remember to clear a flag.
//
// What is deliberately absent. Nothing here says *when* to revisit a topic
// (10d) and nothing counts sessions or streaks (10e). This says what the
// answers show and nothing more.

// The weak-topic rule. Both numbers are part of the definition, so they are
// constants rather than knobs: a client and the assistant both describe a
// topic as "weak" and must mean the same thing by it.
const (
	// WeakTopicMaxCorrectRate is the bar: a topic answered correctly less than
	// 60% of the time is weak. 60% is the line between "mostly right" and
	// "wrong about as often as right" on a four-option question, where
	// guessing alone scores 25%.
	WeakTopicMaxCorrectRate = 0.6
	// WeakTopicMinAnswers is how much evidence a topic needs before it can be
	// called weak. Three, not two, and the arithmetic is the reason: below
	// 60% over at least three answers means more than 1.2 wrong, so a topic is
	// never flagged on fewer than two wrong answers. At two answers one slip
	// is 50% -- which is exactly the "flagged on a single wrong answer" this
	// rule exists to prevent.
	WeakTopicMinAnswers = 3
	// MaxWeakTopics caps one read. It is far more than anybody studies at
	// once, and it is a cap rather than paging because the list is a summary,
	// not a resource to browse.
	MaxWeakTopics = 50
)

// TopicStat is what the user's answers say about one topic, across every quiz
// and every attempt.
type TopicStat struct {
	// Topic is the tag as it is most often written. Questions are grouped
	// case- and space-insensitively -- a model that writes "Mast feed timing"
	// in one quiz and "mast feed timing" in the next has written one topic --
	// and this is the spelling that won.
	Topic string
	// Answers is how many times a question on this topic was answered, over
	// every attempt, finished or not: an answer is graded when it is given,
	// so it is evidence whether or not the attempt was closed. A question
	// left unanswered is not counted, the same rule the score follows.
	Answers int
	// Correct is how many of them were right.
	Correct int
	// Quizzes is how many different quizzes the answers came from.
	Quizzes int
	// StudyPlans and Documents name where the questions came from: the plans
	// their quizzes are filed under and the documents they were made from.
	// They are what lets the assistant connect a topic to "my relay handbook
	// plan" without the user having to name the topic.
	StudyPlans []string
	Documents  []string
	// InActivePlan reports whether any of those plans is still active, which
	// is what "what should I study" prefers.
	InActivePlan bool
	// LastAnswered is when a question on this topic was last answered.
	LastAnswered time.Time
}

// Wrong is how many answers were wrong.
func (t TopicStat) Wrong() int { return t.Answers - t.Correct }

// CorrectRate is the share of answers that were right, in [0, 1].
func (t TopicStat) CorrectRate() float64 {
	if t.Answers == 0 {
		return 0
	}
	return float64(t.Correct) / float64(t.Answers)
}

// CorrectPercent is CorrectRate as a whole percentage, rounded. It is worked
// out here, once, so the API and the assistant say the same number -- and so
// the assistant never has to work it out at all.
func (t TopicStat) CorrectPercent() int { return int(math.Round(t.CorrectRate() * 100)) }

// Weak reports whether the topic meets the weak-topic rule. See the constants.
func (t TopicStat) Weak() bool {
	return t.Answers >= WeakTopicMinAnswers && t.CorrectRate() < WeakTopicMaxCorrectRate
}

// WeakTopicsOf keeps the weak topics and sorts them worst first: lowest
// correct rate, then most answers (more evidence of the same rate is a firmer
// finding), then by name so the order is stable.
func WeakTopicsOf(stats []TopicStat) []TopicStat {
	out := make([]TopicStat, 0, len(stats))
	for _, s := range stats {
		if s.Weak() {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		// Compared as cross-multiplied integers rather than floats, so 1/3
		// and 2/6 tie exactly.
		li, lj := out[i].Correct*out[j].Answers, out[j].Correct*out[i].Answers
		if li != lj {
			return li < lj
		}
		if out[i].Answers != out[j].Answers {
			return out[i].Answers > out[j].Answers
		}
		return out[i].Topic < out[j].Topic
	})
	if len(out) > MaxWeakTopics {
		out = out[:MaxWeakTopics]
	}
	return out
}

// WeakTopics is the caller's weak topics, worst first.
//
// The threshold is applied here rather than in SQL so the rule is one Go
// function a test can pin, and so the repository's query is the plain
// aggregate -- every topic with its counts -- that it is.
func (s *Service) WeakTopics(ctx context.Context, userID uuid.UUID) ([]TopicStat, error) {
	stats, err := s.store.TopicStats(ctx, userID)
	if err != nil {
		return nil, err
	}
	return WeakTopicsOf(stats), nil
}

// TopicStats aggregates the caller's answers by question topic.
//
// Every table is reached through a join that names the owner: quiz_answers
// has no user_id, so the attempt's is the scope, and the quiz must be the same
// user's as the attempt. A question with no topic is left out -- 10b keeps it
// null rather than invent one, and grouping the nulls together would make an
// "untagged" topic that is not a topic of anything.
func (r *Repository) TopicStats(ctx context.Context, userID uuid.UUID) ([]TopicStat, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT mode() WITHIN GROUP (ORDER BY btrim(qq.topic)) AS topic,
		       count(*) AS answers,
		       count(*) FILTER (WHERE ans.correct) AS correct,
		       count(DISTINCT z.id) AS quizzes,
		       coalesce(array_to_json(array_agg(DISTINCT p.title) FILTER (WHERE p.title IS NOT NULL)), '[]'::json),
		       coalesce(array_to_json(array_agg(DISTINCT d.filename) FILTER (WHERE d.filename IS NOT NULL)), '[]'::json),
		       coalesce(bool_or(p.status = 'active'), false) AS in_active_plan,
		       max(ans.created_at) AS last_answered
		FROM quiz_answers ans
		JOIN quiz_attempts a ON a.id = ans.attempt_id
		JOIN quiz_questions qq ON qq.id = ans.question_id
		JOIN quizzes z ON z.id = qq.quiz_id AND z.user_id = a.user_id
		LEFT JOIN study_plans p ON p.id = z.study_plan_id AND p.user_id = z.user_id
		LEFT JOIN documents d ON d.id = z.document_id AND d.user_id = z.user_id
		WHERE a.user_id = $1 AND qq.topic IS NOT NULL AND btrim(qq.topic) <> ''
		GROUP BY lower(btrim(qq.topic))
		ORDER BY lower(btrim(qq.topic))`, userID)
	if err != nil {
		return nil, fmt.Errorf("select topic stats: %w", err)
	}
	defer rows.Close()

	out := []TopicStat{}
	for rows.Next() {
		var (
			t          TopicStat
			plans, doc db.TextArray
		)
		if err := rows.Scan(&t.Topic, &t.Answers, &t.Correct, &t.Quizzes,
			&plans, &doc, &t.InActivePlan, &t.LastAnswered); err != nil {
			return nil, fmt.Errorf("scan topic stats: %w", err)
		}
		t.StudyPlans, t.Documents = plans, doc
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate topic stats: %w", err)
	}
	return out, nil
}
