package study

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jashveer/lifeos/backend/internal/httpx"
)

// StudyRoutes returns the subtree mounted at /study: reads across the whole
// study module rather than about one plan or quiz. It assumes
// auth.RequireAuth is already in front of it.
func (h *Handler) StudyRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/weak-topics", h.WeakTopics)
	return r
}

// weakTopicResponse is one weak topic on the wire.
//
// The counts are carried beside the rate, and the rate beside the percentage,
// so a client can show "1 of 5 correct (20%)" without doing arithmetic and
// can check the rate against the counts it came from.
type weakTopicResponse struct {
	Topic          string   `json:"topic"`
	Answers        int      `json:"answers"`
	Correct        int      `json:"correct"`
	Wrong          int      `json:"wrong"`
	CorrectRate    float64  `json:"correct_rate"`
	CorrectPercent int      `json:"correct_percent"`
	Quizzes        int      `json:"quizzes"`
	StudyPlans     []string `json:"study_plans"`
	Documents      []string `json:"documents"`
	LastAnsweredAt string   `json:"last_answered_at"`
}

// weakTopicsResponse carries the rule with the result, so a client that says
// "weak" can say what it means by it.
type weakTopicsResponse struct {
	WeakTopics []weakTopicResponse `json:"weak_topics"`
	Count      int                 `json:"count"`
	Threshold  struct {
		MaxCorrectRate float64 `json:"max_correct_rate"`
		MinAnswers     int     `json:"min_answers"`
	} `json:"threshold"`
}

func toWeakTopicResponse(t TopicStat) weakTopicResponse {
	return weakTopicResponse{
		Topic: t.Topic, Answers: t.Answers, Correct: t.Correct, Wrong: t.Wrong(),
		// Rounded for the wire: 0.3333333333333333 is noise to a client.
		CorrectRate:    float64(int(t.CorrectRate()*10_000+0.5)) / 10_000,
		CorrectPercent: t.CorrectPercent(), Quizzes: t.Quizzes,
		StudyPlans: nonNil(t.StudyPlans), Documents: nonNil(t.Documents),
		LastAnsweredAt: t.LastAnswered.UTC().Format(httpx.TimeFormat),
	}
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// WeakTopics handles GET /api/v1/study/weak-topics: the topics the caller's
// quiz answers are below the bar on, worst first.
func (h *Handler) WeakTopics(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Authentication required.")
		return
	}
	found, err := h.svc.WeakTopics(r.Context(), userID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := weakTopicsResponse{WeakTopics: make([]weakTopicResponse, 0, len(found)), Count: len(found)}
	out.Threshold.MaxCorrectRate = WeakTopicMaxCorrectRate
	out.Threshold.MinAnswers = WeakTopicMinAnswers
	for _, t := range found {
		out.WeakTopics = append(out.WeakTopics, toWeakTopicResponse(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
